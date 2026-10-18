//go:build integration

// Package e2e exercises the API and worker together against a real (embedded)
// Postgres, with Pub/Sub push simulated in-process.
//
//	go test -tags integration ./internal/e2e/ -v
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/asyncsvc/internal/api"
	"github.com/example/asyncsvc/internal/auth"
	"github.com/example/asyncsvc/internal/blob"
	"github.com/example/asyncsvc/internal/db"
	"github.com/example/asyncsvc/internal/mail"
	"github.com/example/asyncsvc/internal/messages"
	"github.com/example/asyncsvc/internal/outbox"
	"github.com/example/asyncsvc/internal/reports"
	"github.com/example/asyncsvc/internal/worker"
)

// fakePubSub records published messages so the test can push them to the worker.
type fakePubSub struct {
	mu   sync.Mutex
	fail bool
	msgs []published
	seq  int
}

type published struct {
	topic string
	data  []byte
	attrs map[string]string
	id    string
}

func (f *fakePubSub) Publish(_ context.Context, topic string, data []byte, attrs map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", errors.New("pubsub unavailable")
	}
	f.seq++
	id := fmt.Sprint(f.seq)
	f.msgs = append(f.msgs, published{topic, data, attrs, id})
	return id, nil
}

func (f *fakePubSub) take(topic string) []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out, rest []published
	for _, m := range f.msgs {
		if m.topic == topic {
			out = append(out, m)
		} else {
			rest = append(rest, m)
		}
	}
	f.msgs = rest
	return out
}

type captureMailer struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (c *captureMailer) Send(_ context.Context, m mail.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

func (c *captureMailer) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.sent) }

// flakySource fails until failures reaches zero, then delegates.
type flakySource struct {
	mu       sync.Mutex
	failures int
	inner    reports.Source
}

func (s *flakySource) Generate(ctx context.Context, kind string, p reports.Params, w io.Writer) (int64, error) {
	s.mu.Lock()
	if s.failures != 0 {
		s.failures--
		s.mu.Unlock()
		return 0, errors.New("bigquery: backend error")
	}
	s.mu.Unlock()
	return s.inner.Generate(ctx, kind, p, w)
}

type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	ps      *fakePubSub
	mailer  *captureMailer
	source  *flakySource
	api     *httptest.Server
	worker  *httptest.Server
	relay   *outbox.Relay
	reports string
}

// Test identities: bearer token -> user.
var users = auth.Static{
	"alice-token": {UID: "alice", Email: "alice@example.com", EmailVerified: true},
	"bob-token":   {UID: "bob", Email: "bob@example.com", EmailVerified: true},
	"unverified":  {UID: "carol", Email: "carol@example.com", EmailVerified: false},
	"admin-token": {UID: "root", Email: "admin@example.com", EmailVerified: true, Admin: true},
}

var webCfg = func() api.WebConfig {
	var w api.WebConfig
	w.Firebase.APIKey = "test-key"
	w.Firebase.AuthDomain = "demo.firebaseapp.com"
	w.Firebase.ProjectID = "demo"
	return w
}()

// call makes an API request as the user identified by token ("" = anonymous).
func (e *env) call(method, path, token, body string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func freePort(t *testing.T) uint32 {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	port := freePort(t)
	tmp := t.TempDir()
	cache, _ := os.UserCacheDir()
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).Port(port).
		RuntimePath(filepath.Join(tmp, "pg")).
		CachePath(filepath.Join(cache, "embedded-postgres")).
		Logger(io.Discard))
	if err := pg.Start(); err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { pg.Stop() })

	pool, err := db.Connect(ctx, fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil { // idempotent
		t.Fatal(err)
	}

	e := &env{t: t, pool: pool, ps: &fakePubSub{}, mailer: &captureMailer{}, reports: filepath.Join(tmp, "reports")}
	e.source = &flakySource{inner: reports.FakeSource{}}
	store := blob.NewLocal(e.reports)
	repo := &reports.Repo{Pool: pool}
	e.relay = &outbox.Relay{Pool: pool, Pub: e.ps}

	// Mimics a Next.js static export.
	webDir := filepath.Join(tmp, "web")
	os.MkdirAll(filepath.Join(webDir, "_next", "static", "chunks"), 0o755)
	os.WriteFile(filepath.Join(webDir, "index.html"), []byte(`<!doctype html><main>dashboard</main><script src="/_next/static/chunks/a.js"></script><script>self.__next_f=[]</script>`), 0o644)
	os.WriteFile(filepath.Join(webDir, "login.html"), []byte(`<!doctype html><main>login</main>`), 0o644)
	os.WriteFile(filepath.Join(webDir, "404.html"), []byte(`<!doctype html><main>not found page</main>`), 0o644)
	os.WriteFile(filepath.Join(webDir, "_next", "static", "chunks", "a.js"), []byte("console.log(1)"), 0o644)

	e.api = httptest.NewServer((&api.Server{
		Pool: pool, Reports: repo, Relay: e.relay, Pub: e.ps, Store: store,
		TopicReports: "report-requests", TopicEvents: "events", WebDir: webDir,
		Auth: users, Web: webCfg,
	}).Routes())
	t.Cleanup(e.api.Close)

	e.worker = httptest.NewServer((&worker.Worker{
		Pool: pool, Reports: repo, Source: e.source, Store: store, Mailer: e.mailer,
		Relay: e.relay, TopicEmails: "email-requests", PublicAPIURL: "https://api.example.com",
		MaxAttempts: 3, GenerateTimeout: 10 * time.Second, StaleAfter: time.Minute,
	}).Routes())
	t.Cleanup(e.worker.Close)
	return e
}

// push delivers a message to the worker as Pub/Sub would and returns the status.
func (e *env) push(path string, m published, attempt int) int {
	body, _ := json.Marshal(map[string]any{
		"message":         map[string]any{"data": m.data, "attributes": m.attrs, "messageId": m.id},
		"subscription":    "projects/test/subscriptions/x",
		"deliveryAttempt": attempt,
	})
	resp, err := http.Post(e.worker.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) one(topic string) published {
	e.t.Helper()
	msgs := e.ps.take(topic)
	if len(msgs) != 1 {
		e.t.Fatalf("want 1 message on %s, got %d", topic, len(msgs))
	}
	return msgs[0]
}

func (e *env) createReport(body string) (int, map[string]any, http.Header) {
	return e.createReportAs("alice-token", body)
}

func (e *env) createReportAs(token, body string) (int, map[string]any, http.Header) {
	resp, b := e.call("POST", "/v1/reports", token, body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return resp.StatusCode, out, resp.Header
}

func (e *env) getReport(id string) map[string]any {
	_, b := e.call("GET", "/v1/reports/"+id, "alice-token", "")
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

func TestReportHappyPathWithDuplicates(t *testing.T) {
	e := setup(t)

	code, rep, hdr := e.createReport(`{"kind":"daily_active_users","from":"2026-01-01","to":"2026-01-08","notify":true}`)
	if code != http.StatusAccepted {
		t.Fatalf("create: %d %v", code, rep)
	}
	id := rep["id"].(string)
	if hdr.Get("Location") != "/v1/reports/"+id {
		t.Fatalf("location header = %q", hdr.Get("Location"))
	}

	msg := e.one("report-requests")
	if msg.attrs[messages.AttrOutboxID] == "" {
		t.Fatal("missing outbox_id attribute")
	}
	if got := e.push("/pubsub/reports", msg, 1); got != http.StatusNoContent {
		t.Fatalf("report push: %d", got)
	}

	got := e.getReport(id)
	if got["status"] != "ready" || got["row_count"].(float64) != 7 {
		t.Fatalf("report after run: %v", got)
	}
	resp, csv := e.call("GET", got["download_url"].(string), "", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(string(csv), "day,active_users,events\n2026-01-01,") {
		t.Fatalf("csv: %d %q", resp.StatusCode, csv)
	}

	// Duplicate delivery of a finished report is acked without regenerating.
	if got := e.push("/pubsub/reports", msg, 2); got != http.StatusNoContent {
		t.Fatalf("duplicate report push: %d", got)
	}
	if n := len(e.ps.take("email-requests")); n != 1 {
		t.Fatalf("want exactly 1 email request after duplicate, got %d", n)
	}
}

func TestEmailDedupe(t *testing.T) {
	e := setup(t)
	e.createReport(`{"kind":"events_by_type","from":"2026-01-01","to":"2026-01-02","notify":true}`)
	e.push("/pubsub/reports", e.one("report-requests"), 1)
	em := e.one("email-requests")

	for i := 1; i <= 3; i++ {
		if got := e.push("/pubsub/emails", em, i); got != http.StatusNoContent {
			t.Fatalf("email push %d: %d", i, got)
		}
	}
	if e.mailer.count() != 1 {
		t.Fatalf("want 1 email sent, got %d", e.mailer.count())
	}
	if s := e.mailer.sent[0]; s.To != "alice@example.com" || !strings.Contains(s.Text, "https://api.example.com/?report=") {
		t.Fatalf("email: %+v", s)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	e := setup(t)
	e.source.failures = 1
	_, rep, _ := e.createReport(`{"kind":"raw_events","from":"2026-02-01","to":"2026-02-03"}`)
	msg := e.one("report-requests")

	if got := e.push("/pubsub/reports", msg, 1); got != http.StatusServiceUnavailable {
		t.Fatalf("first attempt should nack, got %d", got)
	}
	if s := e.getReport(rep["id"].(string)); s["status"] != "pending" || s["error"] == nil {
		t.Fatalf("after failed attempt: %v", s)
	}
	if got := e.push("/pubsub/reports", msg, 2); got != http.StatusNoContent {
		t.Fatalf("second attempt: %d", got)
	}
	if s := e.getReport(rep["id"].(string)); s["status"] != "ready" || s["attempts"].(float64) != 2 {
		t.Fatalf("after retry: %v", s)
	}
}

func TestFinalAttemptFailsAndNotifies(t *testing.T) {
	e := setup(t)
	e.source.failures = -1 // always fail
	_, rep, _ := e.createReport(`{"kind":"raw_events","from":"2026-02-01","to":"2026-02-03","notify":true}`)
	msg := e.one("report-requests")

	e.push("/pubsub/reports", msg, 1)
	e.push("/pubsub/reports", msg, 2)
	// Final attempt: report marked failed, but still nacked -> dead-letter topic.
	if got := e.push("/pubsub/reports", msg, 3); got != http.StatusServiceUnavailable {
		t.Fatalf("final attempt should nack for DLQ, got %d", got)
	}
	if s := e.getReport(rep["id"].(string)); s["status"] != "failed" {
		t.Fatalf("after final attempt: %v", s)
	}
	em := e.one("email-requests")
	var req messages.EmailRequested
	json.Unmarshal(em.data, &req)
	if req.Template != "report_failed" {
		t.Fatalf("want report_failed email, got %+v", req)
	}
	// Redelivery after it's failed is just acked.
	if got := e.push("/pubsub/reports", msg, 4); got != http.StatusNoContent {
		t.Fatalf("post-failure redelivery: %d", got)
	}
}

func TestOutboxSweepRecoversFromPublishFailure(t *testing.T) {
	e := setup(t)
	e.ps.fail = true
	code, _, _ := e.createReport(`{"kind":"events_by_type","from":"2026-01-01","to":"2026-01-02"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create should still succeed when Pub/Sub is down: %d", code)
	}
	if n := len(e.ps.take("report-requests")); n != 0 {
		t.Fatalf("nothing should be published yet, got %d", n)
	}

	e.ps.fail = false
	resp, err := http.Post(e.worker.URL+"/tasks/relay-outbox", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("relay: %v %v", err, resp)
	}
	e.one("report-requests")

	var pending int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("outbox backlog = %d", pending)
	}
}

func TestValidationAndEvents(t *testing.T) {
	e := setup(t)
	for _, body := range []string{
		`{"kind":"nope","from":"2026-01-01","to":"2026-01-02"}`,
		`{"kind":"raw_events","from":"2026-01-05","to":"2026-01-02"}`,
	} {
		if code, _, _ := e.createReport(body); code != http.StatusBadRequest {
			t.Errorf("want 400 for %s, got %d", body, code)
		}
	}

	resp, _ := e.call("POST", "/v1/events", "admin-token",
		`[{"event_type":"signup","user_id":"u1","properties":{"plan":"pro"}},{"event_type":"login","user_id":"u1","occurred_at":"2026-03-01T10:00:00Z"}]`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("events: %v", resp.StatusCode)
	}
	evs := e.ps.take("events")
	if len(evs) != 2 {
		t.Fatalf("want 2 events published, got %d", len(evs))
	}
	var ev messages.Event
	json.Unmarshal(evs[1].data, &ev)
	if ev.OccurredAt != time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMicro() {
		t.Fatalf("occurred_at micros = %d", ev.OccurredAt)
	}
	json.Unmarshal(evs[0].data, &ev)
	if ev.Properties != `{"plan":"pro"}` {
		t.Fatalf("properties = %q", ev.Properties)
	}

	if r := e.getReport("00000000-0000-0000-0000-000000000000"); r["error"] != "report not found" {
		t.Fatalf("missing report: %v", r)
	}
}

func TestListAndDownload(t *testing.T) {
	e := setup(t)
	_, first, _ := e.createReport(`{"kind":"daily_active_users","from":"2026-01-01","to":"2026-01-04"}`)
	_, second, _ := e.createReport(`{"kind":"events_by_type","from":"2026-01-01","to":"2026-01-02"}`)
	msgs := e.ps.take("report-requests")

	if r := e.getReport(first["id"].(string)); r["download_url"] != nil {
		t.Fatalf("no download link before ready: %v", r)
	}
	e.push("/pubsub/reports", msgs[0], 1)

	_, b := e.call("GET", "/v1/reports?limit=10", "alice-token", "")
	var list struct{ Reports []map[string]any }
	json.Unmarshal(b, &list)
	if len(list.Reports) != 2 || list.Reports[0]["id"] != second["id"] {
		t.Fatalf("list should be newest first: %v", list.Reports)
	}
	if list.Reports[1]["status"] != "ready" || list.Reports[0]["status"] != "pending" {
		t.Fatalf("list statuses: %v", list.Reports)
	}

	// The signed link works without an Authorization header...
	link := e.getReport(first["id"].(string))["download_url"].(string)
	resp, err := http.Get(e.api.URL + link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") ||
		!strings.HasPrefix(string(body), "day,active_users,events") {
		t.Fatalf("download: %d %v %q", resp.StatusCode, resp.Header, body)
	}
	// Excel and PDF versions are generated alongside the CSV.
	rep := e.getReport(first["id"].(string))
	if f, _ := json.Marshal(rep["formats"]); string(f) != `["csv","xlsx","pdf"]` {
		t.Fatalf("formats = %s", f)
	}
	downloads := rep["downloads"].(map[string]any)
	for format, want := range map[string]struct{ contentType, magic string }{
		"csv":  {"text/csv", "day,"},
		"xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "PK"}, // zip container
		"pdf":  {"application/pdf", "%PDF-"},
	} {
		resp, body := e.call("GET", downloads[format].(string), "", "")
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != want.contentType || !strings.HasPrefix(string(body), want.magic) {
			t.Errorf("%s download: %d %q %q", format, resp.StatusCode, resp.Header.Get("Content-Type"), body[:min(8, len(body))])
		}
		if !strings.Contains(resp.Header.Get("Content-Disposition"), "."+format) {
			t.Errorf("%s filename: %q", format, resp.Header.Get("Content-Disposition"))
		}
	}

	// ...but not if it's tampered with.
	resp, _ = http.Get(e.api.URL + strings.Replace(link, "sig=", "sig=0", 1))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered link should be 403, got %d", resp.StatusCode)
	}

	if resp, _ := e.call("GET", "/v1/reports?limit=500", "alice-token", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=500 should be rejected, got %d", resp.StatusCode)
	}
}

func TestAuthorization(t *testing.T) {
	e := setup(t)

	// Anonymous and bad tokens are rejected on every /v1 endpoint.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/me"}, {"GET", "/v1/reports"}, {"POST", "/v1/reports"},
		{"GET", "/v1/reports/00000000-0000-0000-0000-000000000000"}, {"POST", "/v1/events"},
	} {
		for _, tok := range []string{"", "forged"} {
			if resp, _ := e.call(c.method, c.path, tok, ""); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: want 401, got %d", c.method, c.path, tok, resp.StatusCode)
			}
		}
	}
	// Public endpoints stay public.
	if resp, b := e.call("GET", "/config.json", "", ""); resp.StatusCode != 200 || !strings.Contains(string(b), "demo.firebaseapp.com") {
		t.Fatalf("config.json: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.call("GET", "/version", "", ""); resp.StatusCode != 200 || !strings.Contains(string(b), `"commit":`) {
		t.Fatalf("version: %d %s", resp.StatusCode, b)
	}
	for _, p := range []string{"/health", "/healthz"} {
		if resp, _ := e.call("GET", p, "", ""); resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}

	_, me := e.call("GET", "/v1/me", "admin-token", "")
	if !strings.Contains(string(me), `"admin":true`) {
		t.Fatalf("me: %s", me)
	}

	_, alices, _ := e.createReportAs("alice-token", `{"kind":"raw_events","from":"2026-01-01","to":"2026-01-02"}`)
	e.createReportAs("bob-token", `{"kind":"raw_events","from":"2026-01-01","to":"2026-01-02"}`)
	id := alices["id"].(string)
	if alices["owner_email"] != "alice@example.com" {
		t.Fatalf("owner not recorded: %v", alices)
	}

	// Bob can't see Alice's report; it looks like it doesn't exist.
	if resp, _ := e.call("GET", "/v1/reports/"+id, "bob-token", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob reading alice's report: want 404, got %d", resp.StatusCode)
	}
	count := func(token, query string) int {
		_, b := e.call("GET", "/v1/reports"+query, token, "")
		var l struct{ Reports []map[string]any }
		json.Unmarshal(b, &l)
		return len(l.Reports)
	}
	if n := count("bob-token", ""); n != 1 {
		t.Fatalf("bob should list only his report, got %d", n)
	}
	if resp, _ := e.call("GET", "/v1/reports?scope=all", "bob-token", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin scope=all: want 403, got %d", resp.StatusCode)
	}

	// Admin sees everything.
	if n := count("admin-token", "?scope=all"); n != 2 {
		t.Fatalf("admin scope=all: want 2, got %d", n)
	}
	if resp, _ := e.call("GET", "/v1/reports/"+id, "admin-token", ""); resp.StatusCode != 200 {
		t.Fatalf("admin reading alice's report: %d", resp.StatusCode)
	}

	// Events are admin-only.
	if resp, _ := e.call("POST", "/v1/events", "alice-token", `[{"event_type":"x","user_id":"u"}]`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin events: want 403, got %d", resp.StatusCode)
	}

	// Notifications need a verified email, and always go to the caller.
	if code, _, _ := e.createReportAs("unverified", `{"kind":"raw_events","from":"2026-01-01","to":"2026-01-02","notify":true}`); code != http.StatusBadRequest {
		t.Fatalf("unverified notify: want 400, got %d", code)
	}
	_, rep, _ := e.createReportAs("bob-token", `{"kind":"raw_events","from":"2026-01-01","to":"2026-01-02","notify":true,"notify_email":"victim@example.com"}`)
	if rep["notify_email"] != "bob@example.com" {
		t.Fatalf("notify_email must be the caller's own: %v", rep)
	}
}

func TestServesWebApp(t *testing.T) {
	e := setup(t)

	resp, body := e.call("GET", "/", "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "dashboard") || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	// The inline script is allowed by hash, not by 'unsafe-inline'.
	sum := sha256.Sum256([]byte("self.__next_f=[]"))
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || strings.Contains(csp, "unsafe-inline' https://apis") {
		t.Fatalf("CSP should allow the inline script by hash: %s", csp)
	}
	if !strings.Contains(csp, "https://demo.firebaseapp.com") || !strings.Contains(csp, "identitytoolkit.googleapis.com") {
		t.Fatalf("CSP should allow Firebase Auth: %s", csp)
	}

	// Exported routes resolve to <route>.html.
	if resp, body := e.call("GET", "/login", "", ""); resp.StatusCode != 200 || !strings.Contains(string(body), "login") {
		t.Fatalf("/login: %d %q", resp.StatusCode, body)
	}

	resp, body = e.call("GET", "/_next/static/chunks/a.js", "", "")
	if resp.StatusCode != 200 || string(body) != "console.log(1)" || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %q %v", resp.StatusCode, body, resp.Header)
	}

	// Unknown pages get the exported 404 page with a real 404 status;
	// unknown API paths stay JSON; traversal can't escape the web dir.
	if resp, body := e.call("GET", "/no/such/page", "", ""); resp.StatusCode != 404 || !strings.Contains(string(body), "not found page") {
		t.Fatalf("unknown page: %d %q", resp.StatusCode, body)
	}
	if resp, body := e.call("GET", "/v1/unknown", "", ""); resp.StatusCode != 404 || !strings.Contains(string(body), `"error"`) {
		t.Fatalf("unknown api path: %d %q", resp.StatusCode, body)
	}
	if resp, body := e.call("GET", "/../../etc/passwd", "", ""); resp.StatusCode != 404 || strings.Contains(string(body), "root:") {
		t.Fatalf("traversal: %d %q", resp.StatusCode, body)
	}
}
