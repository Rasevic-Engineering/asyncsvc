// Package api is the public REST API. It never does slow work inline: it
// records intent in Postgres (plus an outbox message) and returns 202, or
// publishes straight to Pub/Sub for fire-and-forget analytics events.
//
// Every /v1 endpoint requires a Firebase ID token. Authorization:
//
//	POST /v1/reports            any user; the report is owned by the caller
//	GET  /v1/reports            caller's reports; admins may pass scope=all
//	GET  /v1/reports/{id}       owner or admin (others get 404)
//	POST /v1/events             admin only
//	*    /v1/admin/replays...   admin only (StatsBomb replays: start, stop, delete)
//	GET  /v1/me                 any user
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/asyncsvc/internal/activity"
	"github.com/example/asyncsvc/internal/auth"
	"github.com/example/asyncsvc/internal/blob"
	"github.com/example/asyncsvc/internal/httpx"
	"github.com/example/asyncsvc/internal/ingest"
	"github.com/example/asyncsvc/internal/messages"
	"github.com/example/asyncsvc/internal/outbox"
	"github.com/example/asyncsvc/internal/reports"
	"github.com/example/asyncsvc/internal/version"
)

type Server struct {
	Pool         *pgxpool.Pool
	Reports      *reports.Repo
	Relay        *outbox.Relay
	Pub          outbox.Publisher
	Store        blob.Store
	Auth         auth.Verifier
	TopicReports string
	TopicEvents  string
	// Admin commands for the ingester (StatsBomb replays).
	TopicIngestCommands string
	// WebDir, if set, holds the built React app, served at "/".
	WebDir string
	// Web is the public config the React app loads from /config.json.
	Web WebConfig
	// Activity records published analytics events.
	Activity *activity.Recorder
	// FirebaseHandlerHost, if set (e.g. my-project.firebaseapp.com), makes
	// the API proxy Firebase's /__/auth/ sign-in helper, so Web.Firebase.
	// AuthDomain can be this app's own domain.
	FirebaseHandlerHost string
}

// WebConfig is served to the browser, so it must contain only public values.
// Firebase web API keys identify the project; they are not secrets.
type WebConfig struct {
	Firebase struct {
		APIKey     string `json:"apiKey"`
		AuthDomain string `json:"authDomain"`
		ProjectID  string `json:"projectId"`
	} `json:"firebase"`
	// Set when using the Firebase Auth emulator, e.g. http://localhost:9099.
	AuthEmulatorURL string `json:"authEmulatorUrl,omitempty"`
	// Set when using the Firestore emulator, e.g. localhost:8088.
	FirestoreEmulatorHost string `json:"firestoreEmulatorHost,omitempty"`
	GoogleSignIn          bool   `json:"googleSignIn"`
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	user := func(h http.HandlerFunc) http.HandlerFunc { return auth.Require(s.Auth, h) }
	admin := func(h http.HandlerFunc) http.HandlerFunc { return auth.Require(s.Auth, auth.RequireAdmin(h)) }

	// Public.
	mux.HandleFunc("GET /health", httpx.Health)
	mux.HandleFunc("GET /healthz", httpx.Health)
	mux.HandleFunc("GET /version", version.Handler)
	mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		httpx.JSON(w, http.StatusOK, s.Web)
	})

	// Authenticated.
	mux.HandleFunc("GET /v1/me", user(s.me))
	mux.HandleFunc("POST /v1/reports", user(s.createReport))
	mux.HandleFunc("GET /v1/reports", user(s.listReports))
	mux.HandleFunc("GET /v1/reports/{id}", user(s.getReport))
	mux.HandleFunc("POST /v1/events", admin(s.ingestEvents))
	mux.HandleFunc("GET /v1/admin/replays/catalog", admin(s.replayCatalog))
	mux.HandleFunc("POST /v1/admin/replays", admin(s.startReplay))
	mux.HandleFunc("POST /v1/admin/replays/{id}/stop", admin(s.stopReplay))
	mux.HandleFunc("DELETE /v1/admin/replays/{id}", admin(s.deleteReplay))

	if s.FirebaseHandlerHost != "" {
		// Explicit methods: a method-less pattern would conflict with "GET /".
		proxy := firebaseAuthProxy(s.FirebaseHandlerHost)
		mux.Handle("GET /__/auth/", proxy)
		mux.Handle("POST /__/auth/", proxy) // some identity providers post back
		mux.Handle("GET /__/firebase/", proxy)
	}

	// Local file store only: serves its HMAC-signed download links.
	if h, ok := s.Store.(http.Handler); ok {
		mux.Handle("GET "+blob.FilesPrefix, h)
	}
	if s.WebDir != "" {
		mux.Handle("GET /", spaHandler(s.WebDir, s.Web))
	}
	return mux
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	httpx.JSON(w, http.StatusOK, u)
}

type createReportReq struct {
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
	// Email the caller's own address when the report finishes. Notifications
	// only ever go to the signed-in user, never to an arbitrary address.
	Notify bool `json:"notify"`
}

// createReport stores the report request and its Pub/Sub message in one
// transaction, then returns 202 Accepted with a Location to poll.
func (s *Server) createReport(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var req createReportReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	params := reports.Params{From: req.From, To: req.To}
	if err := reports.Validate(req.Kind, params); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	notifyEmail := ""
	if req.Notify {
		if u.Email == "" || !u.EmailVerified {
			httpx.Error(w, http.StatusBadRequest, "verify your email address to get notifications")
			return
		}
		notifyEmail = u.Email
	}

	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer tx.Rollback(ctx)

	rep, err := s.Reports.Create(ctx, tx, reports.Owner{UID: u.UID, Email: u.Email}, req.Kind, params, notifyEmail)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	msgID, err := outbox.Enqueue(ctx, tx, s.TopicReports, messages.ReportRequested{ReportID: rep.ID}, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.fail(w, r, err)
		return
	}

	// Fast path: publish now. If this fails the scheduled sweep publishes it.
	s.Relay.TryFlush(ctx, msgID)

	w.Header().Set("Location", "/v1/reports/"+rep.ID.String())
	httpx.JSON(w, http.StatusAccepted, rep)
}

func (s *Server) listReports(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			httpx.Error(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	owner := u.UID
	switch r.URL.Query().Get("scope") {
	case "", "mine":
	case "all":
		if !u.Admin {
			httpx.Error(w, http.StatusForbidden, "admin access required")
			return
		}
		owner = ""
	default:
		httpx.Error(w, http.StatusBadRequest, "scope must be mine or all")
		return
	}

	list, err := s.Reports.List(r.Context(), owner, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []reports.Report{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"reports": list})
}

type reportResp struct {
	reports.Report
	// Short-lived links that work without an Authorization header, keyed by
	// format (csv, xlsx, pdf). DownloadURL is the CSV, kept for older clients.
	DownloadURL string            `json:"download_url,omitempty"`
	Downloads   map[string]string `json:"downloads,omitempty"`
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "report not found")
		return
	}
	rep, err := s.Reports.Get(r.Context(), id)
	// Someone else's report looks exactly like a missing one, so report IDs
	// can't be probed.
	if errors.Is(err, reports.ErrNotFound) || (err == nil && !u.Admin && !rep.OwnedBy(u.UID)) {
		httpx.Error(w, http.StatusNotFound, "report not found")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	resp := reportResp{Report: rep}
	if rep.Status == reports.StatusReady && rep.FileKey != nil {
		resp.Downloads = map[string]string{}
		for _, f := range rep.Formats {
			url, err := s.Store.URL(r.Context(), reports.FormatKey(*rep.FileKey, f), 5*time.Minute)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			resp.Downloads[f] = url
		}
		resp.DownloadURL = resp.Downloads["csv"]
	} else if rep.Status == reports.StatusPending || rep.Status == reports.StatusRunning {
		w.Header().Set("Retry-After", "5")
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.JSON(w, http.StatusOK, resp)
}

type eventReq struct {
	EventType  string          `json:"event_type"`
	UserID     string          `json:"user_id"`
	OccurredAt *time.Time      `json:"occurred_at"`
	Properties json.RawMessage `json:"properties"`
}

const maxEventsPerRequest = 500

// ingestEvents publishes analytics events to Pub/Sub. A BigQuery subscription
// writes them into the events table, so there is no consumer code to run.
// Analytics events don't need the outbox: there is no DB write to stay
// consistent with, and the client gets an error if Pub/Sub is unavailable.
func (s *Server) ingestEvents(w http.ResponseWriter, r *http.Request) {
	var batch []eventReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20)).Decode(&batch); err != nil {
		httpx.Error(w, http.StatusBadRequest, "body must be a JSON array of events")
		return
	}
	if len(batch) == 0 || len(batch) > maxEventsPerRequest {
		httpx.Error(w, http.StatusBadRequest, "send between 1 and 500 events")
		return
	}

	now := time.Now()
	out := make([]messages.Event, len(batch))
	for i, e := range batch {
		if e.EventType == "" || e.UserID == "" {
			httpx.Error(w, http.StatusBadRequest, "event_type and user_id are required")
			return
		}
		occurred := now
		if e.OccurredAt != nil {
			occurred = *e.OccurredAt
		}
		out[i] = messages.Event{
			EventID:    uuid.NewString(),
			EventType:  e.EventType,
			UserID:     e.UserID,
			OccurredAt: occurred.UnixMicro(),
			ReceivedAt: now.UnixMicro(),
			Source:     "api",
		}
		if len(e.Properties) > 0 && string(e.Properties) != "null" {
			out[i].Properties = string(e.Properties)
		}
	}

	ids := make([]string, len(out))
	types := map[string]int{}
	start := time.Now()
	for i, ev := range out {
		data, _ := json.Marshal(ev)
		if _, err := s.Pub.Publish(r.Context(), s.TopicEvents, data, map[string]string{"event_type": ev.EventType}); err != nil {
			s.fail(w, r, err)
			return
		}
		ids[i] = ev.EventID
		types[ev.EventType]++
	}
	// One activity row per batch (not per event), so a big batch doesn't
	// flood the feed. The BigQuery subscription consumes these directly.
	s.Activity.Record(messages.Activity{
		Kind: messages.ActivityPublished, Topic: s.TopicEvents,
		Summary: summarizeEvents(len(out), types), DurationMs: time.Since(start).Milliseconds(),
	})
	httpx.JSON(w, http.StatusAccepted, map[string]any{"accepted": len(ids), "event_ids": ids})
}

func (s *Server) replayCatalog(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"matches": ingest.Catalog, "speeds": ingest.Speeds})
}

type startReplayReq struct {
	MatchID int `json:"match_id"`
	Speed   int `json:"speed"`
}

// startReplay asks the ingester to replay a StatsBomb match. The ingester
// reports progress in Firestore (ingest/replay-<id>), which the admin Live
// data page listens to.
func (s *Server) startReplay(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var req startReplayReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, ok := ingest.CatalogMatch(req.MatchID); !ok {
		httpx.Error(w, http.StatusBadRequest, "unknown match_id (see /v1/admin/replays/catalog)")
		return
	}
	if !ingest.ValidSpeed(req.Speed) {
		httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("speed must be one of %v", ingest.Speeds))
		return
	}
	cmd := messages.IngestCommand{
		Action: "replay", ReplayID: uuid.NewString(), MatchID: req.MatchID, Speed: req.Speed, RequestedBy: u.Email,
	}
	if !s.sendIngestCommand(w, r, cmd) {
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]string{"replay_id": cmd.ReplayID})
}

func (s *Server) stopReplay(w http.ResponseWriter, r *http.Request) {
	s.replayCommand(w, r, "stop")
}

// deleteReplay asks the ingester to stop the replay (if running) and remove
// it from the Live data page: its status card and highlight stream. Its
// events stay in BigQuery, so reports still include it.
func (s *Server) deleteReplay(w http.ResponseWriter, r *http.Request) {
	s.replayCommand(w, r, "delete")
}

func (s *Server) replayCommand(w http.ResponseWriter, r *http.Request, action string) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "replay not found")
		return
	}
	if !s.sendIngestCommand(w, r, messages.IngestCommand{Action: action, ReplayID: id.String()}) {
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]string{"replay_id": id.String()})
}

func (s *Server) sendIngestCommand(w http.ResponseWriter, r *http.Request, cmd messages.IngestCommand) bool {
	data, _ := json.Marshal(cmd)
	if _, err := s.Pub.Publish(r.Context(), s.TopicIngestCommands, data, map[string]string{"action": cmd.Action}); err != nil {
		s.fail(w, r, err)
		return false
	}
	return true
}

func summarizeEvents(n int, types map[string]int) string {
	names := make([]string, 0, len(types))
	for t, c := range types {
		names = append(names, fmt.Sprintf("%s×%d", t, c))
	}
	sort.Strings(names)
	if len(names) > 3 {
		names = append(names[:3], "…")
	}
	return fmt.Sprintf("%d events → BigQuery (%s)", n, strings.Join(names, ", "))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "request failed", "err", err, "path", r.URL.Path)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}
