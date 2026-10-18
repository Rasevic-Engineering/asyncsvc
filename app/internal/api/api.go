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
//	GET  /v1/me                 any user
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/asyncsvc/internal/auth"
	"github.com/example/asyncsvc/internal/blob"
	"github.com/example/asyncsvc/internal/httpx"
	"github.com/example/asyncsvc/internal/messages"
	"github.com/example/asyncsvc/internal/outbox"
	"github.com/example/asyncsvc/internal/reports"
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
	// WebDir, if set, holds the built React app, served at "/".
	WebDir string
	// Web is the public config the React app loads from /config.json.
	Web WebConfig
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
	GoogleSignIn    bool   `json:"googleSignIn"`
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	user := func(h http.HandlerFunc) http.HandlerFunc { return auth.Require(s.Auth, h) }
	admin := func(h http.HandlerFunc) http.HandlerFunc { return auth.Require(s.Auth, auth.RequireAdmin(h)) }

	// Public.
	mux.HandleFunc("GET /health", httpx.Health)
	mux.HandleFunc("GET /healthz", httpx.Health)
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
	// Short-lived link that works without an Authorization header.
	DownloadURL string `json:"download_url,omitempty"`
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
		url, err := s.Store.URL(r.Context(), *rep.FileKey, 5*time.Minute)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		resp.DownloadURL = url
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
		}
		if len(e.Properties) > 0 && string(e.Properties) != "null" {
			out[i].Properties = string(e.Properties)
		}
	}

	ids := make([]string, len(out))
	for i, ev := range out {
		data, _ := json.Marshal(ev)
		if _, err := s.Pub.Publish(r.Context(), s.TopicEvents, data, map[string]string{"event_type": ev.EventType}); err != nil {
			s.fail(w, r, err)
			return
		}
		ids[i] = ev.EventID
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"accepted": len(ids), "event_ids": ids})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "request failed", "err", err, "path", r.URL.Path)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}
