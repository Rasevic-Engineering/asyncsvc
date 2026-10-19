package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureServer serves saved real API responses (testdata/) by path.
func fixtureServer(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for prefix, file := range routes {
			if strings.HasPrefix(r.URL.Path+"?"+r.URL.RawQuery, prefix) || r.URL.Path == prefix {
				b, err := os.ReadFile("testdata/" + file)
				if err != nil {
					t.Errorf("fixture %s: %v", file, err)
				}
				w.Write(b)
				return
			}
		}
		t.Logf("no fixture for %s", r.URL)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func playByType(t *testing.T, plays []Play, typ string) Play {
	t.Helper()
	for _, p := range plays {
		if p.Type == typ {
			return p
		}
	}
	types := map[string]int{}
	for _, p := range plays {
		types[p.Type]++
	}
	t.Fatalf("no %q play; have %v", typ, types)
	return Play{}
}

func checkSeqUnique(t *testing.T, plays []Play) {
	t.Helper()
	seen := map[int64]bool{}
	for _, p := range plays {
		if seen[p.Seq] {
			t.Fatalf("duplicate seq %d", p.Seq)
		}
		seen[p.Seq] = true
	}
}

func TestNHLAdapter(t *testing.T) {
	srv := fixtureServer(t, map[string]string{"/v1/gamecenter/": "nhl_pbp.json"})
	n := &NHL{BaseURL: srv.URL}
	cur, plays, err := n.Plays(context.Background(), Game{ID: "2026020135"})
	if err != nil {
		t.Fatal(err)
	}
	// Saved during the 2nd intermission.
	if cur.State != Live || cur.Away.Abbr != "CBJ" || cur.Home.Abbr != "MIN" || cur.StatusText != "P2 intermission" ||
		cur.Extra["home_sog"] != 29 {
		t.Fatalf("game: %+v", cur)
	}
	if len(plays) != 40 {
		t.Fatalf("plays: %d", len(plays))
	}
	checkSeqUnique(t, plays)
	shot := playByType(t, plays, "shot-on-goal")
	if shot.Actor == "" || shot.Team == "" || !strings.Contains(shot.Text, shot.Actor) || shot.Period != "P1" {
		t.Fatalf("shot: %+v", shot)
	}
}

func TestMLBAdapter(t *testing.T) {
	srv := fixtureServer(t, map[string]string{
		"/api/v1/schedule": "mlb_schedule.json",
		"/api/v1.1/game/":  "mlb_feed.json",
	})
	m := &MLB{BaseURL: srv.URL, Today: func() string { return "2026-10-18" }}
	games, err := m.Schedule(context.Background())
	if err != nil || len(games) != 1 {
		t.Fatalf("schedule: %v %v", games, err)
	}
	g := games[0]
	if g.State != Final || g.Away.Abbr != "TB" || g.Home.Abbr != "CLE" || g.Competition != "AL Championship Series" {
		t.Fatalf("schedule game: %+v", g)
	}

	cur, plays, err := m.Plays(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if cur.StatusText != "Final" || cur.Away.Score != 3 || cur.Home.Score != 1 {
		t.Fatalf("game: %+v", cur)
	}
	checkSeqUnique(t, plays)
	pitch := playByType(t, plays, "pitch")
	if pitch.Props["mph"] == nil || pitch.Props["pitch_type"] == "" || !strings.Contains(pitch.Text, "mph") || pitch.Actor == "" {
		t.Fatalf("pitch: %+v", pitch)
	}
	// First at-bat: Yandy Díaz doubles (pitch-by-pitch, then the result).
	double := playByType(t, plays, "double")
	if double.Seq != 999 || double.Actor != "Yandy Díaz" || double.Team != "TB" || !strings.Contains(double.Text, "doubles") {
		t.Fatalf("result: %+v", double)
	}
	for _, p := range plays {
		if p.Seq < 999 && p.Type == "pitch" && p.Team != "CLE" {
			t.Fatalf("top of the 1st: CLE is pitching, got %+v", p)
		}
		break
	}
}

func TestNFLAdapter(t *testing.T) {
	srv := fixtureServer(t, map[string]string{"/football/nfl/summary": "nfl_summary.json"})
	e := NewNFL()
	e.BaseURL = srv.URL
	cur, plays, err := e.Plays(context.Background(), Game{ID: "401872996"})
	if err != nil {
		t.Fatal(err)
	}
	// Houston at Jacksonville, 34–31.
	if cur.State != Final || cur.Away.Abbr != "HOU" || cur.Home.Abbr != "JAX" || cur.Away.Score != 34 || cur.Home.Score != 31 {
		t.Fatalf("game: %+v", cur)
	}
	if len(plays) < 10 {
		t.Fatalf("plays: %d", len(plays))
	}
	checkSeqUnique(t, plays)
	for _, p := range plays {
		if p.Team == "" || p.Text == "" || !strings.HasPrefix(p.Period, "Q") {
			t.Fatalf("play: %+v", p)
		}
	}
	punt := playByType(t, plays, "punt")
	if punt.Props["down_distance"] == "" || !strings.Contains(punt.Text, "punts") {
		t.Fatalf("punt: %+v", punt)
	}
}

func TestNBAAdapter(t *testing.T) {
	srv := fixtureServer(t, map[string]string{"/basketball/nba/summary": "nba_summary.json"})
	e := NewNBA()
	e.BaseURL = srv.URL
	cur, plays, err := e.Plays(context.Background(), Game{ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if cur.State != Final || cur.Home.Abbr == "" || cur.Away.Abbr == "" {
		t.Fatalf("game: %+v", cur)
	}
	if len(plays) < 30 {
		t.Fatalf("plays: %d", len(plays))
	}
	checkSeqUnique(t, plays)
	scoring := 0
	for _, p := range plays {
		if p.Scoring {
			scoring++
			if p.Team == "" {
				t.Fatalf("scoring play without team: %+v", p)
			}
		}
	}
	if scoring == 0 {
		t.Fatal("expected scoring plays")
	}
	ft := playByType(t, plays, "free-throw")
	if ft.Period != "Q1" || ft.Clock == "" {
		t.Fatalf("free throw: %+v", ft)
	}
}

func TestSoccerAdapter(t *testing.T) {
	srv := fixtureServer(t, map[string]string{
		"/soccer/eng.1/scoreboard": "soccer_scoreboard.json",
		"/soccer/eng.1/summary":    "soccer_summary.json",
	})
	e := NewSoccer()
	e.BaseURL = srv.URL
	e.Paths = []string{"soccer/eng.1"}
	games, err := e.Schedule(context.Background())
	if err != nil || len(games) == 0 {
		t.Fatalf("schedule: %d %v", len(games), err)
	}
	cur, plays, err := e.Plays(context.Background(), Game{ID: "401879261", Competition: "English Premier League"})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Home.Abbr != "BHA" || cur.Away.Abbr != "CRY" || cur.Home.Score != 5 {
		t.Fatalf("game: %+v", cur)
	}
	checkSeqUnique(t, plays)
	goal := playByType(t, plays, "goal")
	if !goal.Scoring || goal.Team != "BHA" || goal.Period != "1H" || goal.Clock == "" {
		t.Fatalf("goal: %+v", goal)
	}
	own := playByType(t, plays, "own-goal")
	if !own.Scoring {
		t.Fatalf("own goal: %+v", own)
	}
}

func TestTypeSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Free Throw - 3 of 3": "free-throw", "Pass Reception": "pass-reception", "Home Run": "home-run",
		"Kickoff": "kickoff", "yellow-card": "yellow-card", "  Field Goal Good ": "field-goal-good",
	} {
		if got := typeSlug(in); got != want {
			t.Errorf("typeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLeagues(t *testing.T) {
	ls, err := Leagues("nhl, MLB,nba,nfl,soccer")
	if err != nil || len(ls) != 5 {
		t.Fatalf("%v %v", ls, err)
	}
	if ls, _ := Leagues("none"); len(ls) != 0 {
		t.Fatal("none should disable live leagues")
	}
	if _, err := Leagues("cricket"); err == nil {
		t.Fatal("unknown league should fail")
	}
}

// fakeLeague serves a fixed, growing list of plays.
type fakeLeague struct {
	mu    sync.Mutex
	state string
	plays []Play
	games []Game
}

func (f *fakeLeague) Name() string             { return "nba" }
func (f *fakeLeague) Label() string            { return "NBA" }
func (f *fakeLeague) PollEvery() time.Duration { return 5 * time.Millisecond }
func (f *fakeLeague) Schedule(context.Context) ([]Game, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Game(nil), f.games...), nil
}
func (f *fakeLeague) Plays(_ context.Context, g Game) (Game, []Play, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g.State, g.StatusText = f.state, "Q3 · 4:12"
	g.Home, g.Away = Team{Abbr: "BOS", Score: 70}, Team{Abbr: "DET", Score: 68}
	return g, append([]Play(nil), f.plays...), nil
}

func TestTrackerDedupRestartAndStatus(t *testing.T) {
	pub, status := &fakePub{}, &MemoryStatus{}
	in := newTestIngester(pub, status)
	lg := &fakeLeague{state: Live, plays: []Play{
		{Seq: 2, Type: "jump-ball", Text: "Jump ball"},
		{Seq: 1, Type: "period-start", Text: "Start of Q1"},
		{Seq: 5, Type: "three-point-jumper", Text: "Tatum makes 3", Team: "BOS", Actor: "Jayson Tatum", Scoring: true},
	}}
	g := Game{ID: "401", State: Live}
	var last, n int64 = -1, 0
	if _, err := in.Tracker.poll(context.Background(), lg, g, &last, &n); err != nil {
		t.Fatal(err)
	}
	evs := pub.all()
	if len(evs) != 3 || last != 5 {
		t.Fatalf("first poll: %d events, last %d", len(evs), last)
	}
	for _, e := range evs {
		if !pub.ui[e.EventID] {
			t.Fatalf("live plays must be tagged for the UI: %+v", e)
		}
		if e.Source != "nba" || e.GameID != "nba:401" || !strings.HasPrefix(e.EventType, "nba.") {
			t.Fatalf("event: %+v", e)
		}
	}
	three := byType(t, evs, "nba.three-point-jumper")
	var props map[string]any
	json.Unmarshal([]byte(three.Properties), &props)
	if three.UserID != "Jayson Tatum" || props["scoring"] != true || props["text"] != "Tatum makes 3" || props["game"] != "DET @ BOS" {
		t.Fatalf("three: %+v %v", three, props)
	}
	if a := byType(t, evs, "nba.jump-ball").UserID; a != "game" {
		t.Fatalf("no actor or team → %q", a)
	}
	// Times follow game order (Seq), even within one poll.
	if s1, s2, s5 := byType(t, evs, "nba.period-start").OccurredAt, byType(t, evs, "nba.jump-ball").OccurredAt,
		three.OccurredAt; !(s1 < s2 && s2 < s5) {
		t.Fatalf("times out of game order: %d %d %d", s1, s2, s5)
	}

	doc := status.Snapshot("game-nba-401")
	if doc["kind"] != "game" || doc["league"] != "nba" || doc["status_text"] != "Q3 · 4:12" ||
		doc["home"].(map[string]any)["score"] != 70 || doc["plays_ingested"] != int64(3) {
		t.Fatalf("status: %v", doc)
	}

	// New play only.
	lg.plays = append(lg.plays, Play{Seq: 9, Type: "timeout", Text: "Timeout"})
	in.Tracker.poll(context.Background(), lg, g, &last, &n)
	if len(pub.all()) != 4 {
		t.Fatalf("after new play: %d", len(pub.all()))
	}

	// A restarted tracker resumes from the status store, then stops at final.
	lg.state = Final
	in2 := newTestIngester(pub, status)
	done := make(chan struct{})
	go func() { in2.Tracker.trackGame(context.Background(), lg, g); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("trackGame should return at final")
	}
	if len(pub.all()) != 4 || status.Snapshot("game-nba-401")["state"] != Final {
		t.Fatal("restart re-published or didn't record final")
	}
}

func TestTrackerSchedule(t *testing.T) {
	pub, status := &fakePub{}, &MemoryStatus{}
	in := newTestIngester(pub, status)
	lg := &fakeLeague{state: Live, games: []Game{
		{ID: "1", State: Scheduled, StartTime: "2026-10-20T23:00:00Z", Home: Team{Abbr: "DET"}, Away: Team{Abbr: "BOS"}},
		{ID: "2", State: Live},
	}}
	in.Tracker.Leagues = []League{lg}
	ctx, cancel := context.WithCancel(context.Background())
	in.Tracker.tracking = map[string]bool{}
	var wg sync.WaitGroup
	if err := in.Tracker.checkSchedule(ctx, lg, &wg); err != nil {
		t.Fatal(err)
	}
	if d := status.Snapshot("game-nba-1"); d["state"] != Scheduled || d["start_time"] == nil {
		t.Fatalf("scheduled game: %v", d)
	}
	waitFor(t, func() bool { return status.Snapshot("game-nba-2")["state"] == Live })
	// A second schedule check doesn't start a second tracker for game 2.
	in.Tracker.checkSchedule(ctx, lg, &wg)
	in.Tracker.mu.Lock()
	n := len(in.Tracker.tracking)
	in.Tracker.mu.Unlock()
	if n != 1 {
		t.Fatalf("tracking %d games, want 1", n)
	}
	cancel()
	wg.Wait()
}
