package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/asyncsvc/internal/activity"
	"github.com/example/asyncsvc/internal/messages"
	"github.com/example/asyncsvc/internal/pubsubx"
)

type fakePub struct {
	mu     sync.Mutex
	events []messages.Event
	other  map[string]int
}

func (f *fakePub) Publish(_ context.Context, topic string, data []byte, _ map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if topic != "events" {
		if f.other == nil {
			f.other = map[string]int{}
		}
		f.other[topic]++
		return "x", nil
	}
	var e messages.Event
	if err := json.Unmarshal(data, &e); err != nil {
		return "", err
	}
	f.events = append(f.events, e)
	return "id", nil
}

func (f *fakePub) PublishAsync(topic string, data []byte, attrs map[string]string) {
	f.Publish(context.Background(), topic, data, attrs)
}

func (f *fakePub) all() []messages.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]messages.Event(nil), f.events...)
}

// byType finds the first event of a type. Batches are published
// concurrently, so arrival order isn't meaningful (nor is it in Pub/Sub).
func byType(t *testing.T, evs []messages.Event, typ string) messages.Event {
	t.Helper()
	for _, e := range evs {
		if e.EventType == typ {
			return e
		}
	}
	t.Fatalf("no %s event in %d events", typ, len(evs))
	return messages.Event{}
}

func newTestIngester(pub *fakePub, status *MemoryStatus) *Ingester {
	rec := &activity.Recorder{Pub: pub, Topic: "activity", Service: "ingester"}
	return New(pub, "events", rec, status)
}

// ---- NHL --------------------------------------------------------------

const nhlRoster = `"rosterSpots":[
  {"teamId":30,"playerId":1,"firstName":{"default":"Kirill"},"lastName":{"default":"Kaprizov"}},
  {"teamId":29,"playerId":2,"firstName":{"default":"Elvis"},"lastName":{"default":"Merzlikins"}},
  {"teamId":29,"playerId":3,"firstName":{"default":"Zach"},"lastName":{"default":"Werenski"}}]`

func nhlPlay(eventID, sort int, typ string, details string) string {
	return fmt.Sprintf(`{"eventId":%d,"sortOrder":%d,"periodDescriptor":{"number":1,"periodType":"REG"},
	  "timeInPeriod":"01:%02d","timeRemaining":"18:00","situationCode":"1551","typeDescKey":%q,"details":%s}`,
		eventID, sort, sort, typ, details)
}

func nhlServer(t *testing.T, state *string, plays *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/score/now":
			fmt.Fprintf(w, `{"games":[{"id":2026020135,"gameState":%q,"startTimeUTC":"2026-10-18T22:00:00Z",
			  "awayTeam":{"id":29,"abbrev":"CBJ","score":1},"homeTeam":{"id":30,"abbrev":"MIN","score":2}}]}`, *state)
		case strings.HasSuffix(r.URL.Path, "/play-by-play"):
			fmt.Fprintf(w, `{"id":2026020135,"gameState":%q,"startTimeUTC":"2026-10-18T22:00:00Z",
			  "awayTeam":{"id":29,"abbrev":"CBJ","score":1,"sog":12},"homeTeam":{"id":30,"abbrev":"MIN","score":2,"sog":29},
			  "periodDescriptor":{"number":2,"periodType":"REG"},"clock":{"timeRemaining":"01:10","inIntermission":false},
			  "plays":[%s],%s}`, *state, strings.Join(*plays, ","), nhlRoster)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNHLPublishesOnlyNewPlays(t *testing.T) {
	state := "LIVE"
	plays := []string{
		nhlPlay(10, 1, "faceoff", `{"winningPlayerId":1,"eventOwnerTeamId":30}`),
		nhlPlay(59, 2, "shot-on-goal", `{"shootingPlayerId":3,"goalieInNetId":99,"eventOwnerTeamId":29,"xCoord":39,"yCoord":-24,"shotType":"slap"}`),
		nhlPlay(70, 3, "stoppage", `{}`),
	}
	srv := nhlServer(t, &state, &plays)
	pub, status := &fakePub{}, &MemoryStatus{}
	in := newTestIngester(pub, status)
	in.NHL.BaseURL = srv.URL

	var last, ingested int64 = -1, 0
	if _, err := in.NHL.pollPlays(context.Background(), 2026020135, &last, &ingested); err != nil {
		t.Fatal(err)
	}
	evs := pub.all()
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	shot := byType(t, evs, "nhl.shot-on-goal")
	if shot.UserID != "Zach Werenski" || shot.Team != "CBJ" ||
		shot.Source != "nhl" || shot.GameID != "nhl:2026020135" || shot.OccurredAt == 0 {
		t.Fatalf("shot event: %+v", shot)
	}
	var props map[string]any
	json.Unmarshal([]byte(shot.Properties), &props)
	if props["game"] != "CBJ @ MIN" || props["period"] != float64(1) || props["players"].(map[string]any)["shooting"] != "Zach Werenski" {
		t.Fatalf("shot properties: %v", props)
	}
	if a, b := byType(t, evs, "nhl.faceoff").UserID, byType(t, evs, "nhl.stoppage").UserID; a != "Kirill Kaprizov" || b != "game" {
		t.Fatalf("actors: %q %q", a, b)
	}

	doc := status.Snapshot("nhl-2026020135")
	if doc["home_score"] != 2 || doc["away_sog"] != 12 || doc["plays_ingested"] != int64(3) || doc["clock"] != "01:10" {
		t.Fatalf("status: %v", doc)
	}

	// A new play arrives: only it is published.
	plays = append(plays, nhlPlay(80, 4, "goal", `{"scoringPlayerId":1,"eventOwnerTeamId":30}`))
	in.NHL.pollPlays(context.Background(), 2026020135, &last, &ingested)
	if evs := pub.all(); len(evs) != 4 || byType(t, evs, "nhl.goal").UserID != "Kirill Kaprizov" {
		t.Fatalf("after new play: %d events", len(evs))
	}

	// A restarted ingester picks up from the status store: nothing re-published.
	in2 := newTestIngester(pub, status)
	in2.NHL.BaseURL = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	state = "FINAL"
	done := make(chan struct{})
	go func() { in2.NHL.trackGame(ctx, 2026020135); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("trackGame should return once the game is final")
	}
	cancel()
	if n := len(pub.all()); n != 4 {
		t.Fatalf("restart re-published plays: %d events", n)
	}
	if status.Snapshot("nhl-2026020135")["state"] != "FINAL" {
		t.Fatal("final state not recorded")
	}

	// Same play, same event ID (duplicates are detectable downstream).
	if eventID("nhl", "2026020135", "59") != shot.EventID {
		t.Fatal("event IDs must be deterministic")
	}
}

func TestNHLScheduleRecordsUpcomingGames(t *testing.T) {
	state := "FUT"
	var plays []string
	srv := nhlServer(t, &state, &plays)
	status := &MemoryStatus{}
	in := newTestIngester(&fakePub{}, status)
	in.NHL.BaseURL = srv.URL
	in.NHL.tracking = map[int64]context.CancelFunc{}

	var wg sync.WaitGroup
	if err := in.NHL.checkSchedule(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	doc := status.Snapshot("nhl-2026020135")
	if doc["state"] != "FUT" || doc["away"] != "CBJ" || doc["home"] != "MIN" || doc["start_time"] == nil {
		t.Fatalf("scheduled game status: %v", doc)
	}
	if len(in.NHL.tracking) != 0 {
		t.Fatal("a future game shouldn't be polled")
	}
}

// ---- StatsBomb ----------------------------------------------------------

const sbEvents = `[
 {"id":"a1","index":1,"period":1,"timestamp":"00:00:00.000","minute":0,"second":0,"type":{"name":"Starting XI"},"team":{"name":"Argentina"},"possession_team":{"name":"Argentina"},"play_pattern":{"name":"Regular Play"}},
 {"id":"a2","index":2,"period":1,"timestamp":"00:00:01.000","minute":0,"second":1,"type":{"name":"Pass"},"team":{"name":"Argentina"},"player":{"name":"Lionel Messi"},"location":[60,40],"possession_team":{"name":"Argentina"},"play_pattern":{"name":"From Kick Off"},"pass":{"length":12.5,"recipient":{"name":"Rodrigo De Paul"},"end_location":[70,40]}},
 {"id":"a3","index":3,"period":1,"timestamp":"00:00:01.100","minute":0,"second":1,"type":{"name":"Ball Receipt*"},"team":{"name":"Argentina"},"player":{"name":"Rodrigo De Paul"},"possession_team":{"name":"Argentina"},"play_pattern":{"name":"From Kick Off"}},
 {"id":"a4","index":4,"period":1,"timestamp":"00:20:00.000","minute":20,"second":0,"type":{"name":"Shot"},"team":{"name":"Argentina"},"player":{"name":"Lionel Messi"},"location":[108,40],"possession_team":{"name":"Argentina"},"play_pattern":{"name":"From Free Kick"},"shot":{"statsbomb_xg":0.78,"outcome":{"name":"Goal"},"body_part":{"name":"Left Foot"},"type":{"name":"Penalty"}}},
 {"id":"b1","index":5,"period":2,"timestamp":"00:00:00.000","minute":45,"second":0,"type":{"name":"Half Start"},"team":{"name":"France"},"possession_team":{"name":"France"},"play_pattern":{"name":"Regular Play"}},
 {"id":"b2","index":6,"period":2,"timestamp":"00:35:00.000","minute":80,"second":0,"type":{"name":"Shot"},"team":{"name":"France"},"player":{"name":"Kylian Mbappé"},"location":[100,35],"possession_team":{"name":"France"},"play_pattern":{"name":"Regular Play"},"shot":{"statsbomb_xg":0.3,"outcome":{"name":"Goal"},"body_part":{"name":"Right Foot"},"type":{"name":"Open Play"}}},
 {"id":"c1","index":7,"period":5,"timestamp":"00:01:00.000","minute":120,"second":0,"type":{"name":"Shot"},"team":{"name":"France"},"player":{"name":"Kylian Mbappé"},"possession_team":{"name":"France"},"play_pattern":{"name":"Regular Play"},"shot":{"statsbomb_xg":0.78,"outcome":{"name":"Goal"},"body_part":{"name":"Right Foot"},"type":{"name":"Penalty"}}}
]`

func sbServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/3869685.json" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(sbEvents))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReplay(t *testing.T) {
	pub, status := &fakePub{}, &MemoryStatus{}
	in := newTestIngester(pub, status)
	in.StatsBomb.BaseURL = sbServer(t).URL
	var waits []time.Duration
	in.StatsBomb.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }

	cmd := messages.IngestCommand{Action: "replay", ReplayID: "r1", MatchID: 3869685, Speed: 10, RequestedBy: "admin@example.com"}
	if err := in.StatsBomb.Replay(context.Background(), cmd, 0); err != nil {
		t.Fatal(err)
	}
	evs := pub.all()
	if len(evs) != 7 {
		t.Fatalf("want 7 events, got %d", len(evs))
	}
	pass := byType(t, evs, "soccer.pass")
	if pass.UserID != "Lionel Messi" || pass.Team != "Argentina" ||
		pass.Source != "statsbomb" || pass.GameID != "statsbomb:3869685" {
		t.Fatalf("pass event: %+v", pass)
	}
	byType(t, evs, "soccer.ball-receipt") // "Ball Receipt*" slugged
	if a := byType(t, evs, "soccer.starting-xi").UserID; a != "team:Argentina" {
		t.Fatalf("team actor: %q", a)
	}
	var shot map[string]any
	for _, e := range evs {
		if e.EventType == "soccer.shot" && e.Team == "Argentina" {
			json.Unmarshal([]byte(e.Properties), &shot)
		}
	}
	if shot["xg"] != 0.78 || shot["outcome"] != "Goal" || shot["match"] != "Argentina 3–3 France (pens 4–2)" || shot["replay_id"] != "r1" {
		t.Fatalf("shot properties: %v", shot)
	}

	// Timing at 10×: 1s→0.1s and 0.1s→0.01s are batched but still owed,
	// then 20min (capped to 5s) → one wait of 5.11s; period change → 2s
	// pause; 35min → capped 5s; period change → 2s.
	want := []time.Duration{maxGap + 110*time.Millisecond, periodPause, maxGap, periodPause}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}

	doc := status.Snapshot("replay-r1")
	if doc["state"] != "finished" || doc["emitted"] != 7 || doc["total"] != 7 || doc["requested_by"] != "admin@example.com" {
		t.Fatalf("status: %v", doc)
	}
	// Shootout penalties (period 5) don't count toward the score.
	if s := doc["score"].(map[string]int); s["Argentina"] != 1 || s["France"] != 1 {
		t.Fatalf("score: %v", s)
	}
	if pub.other["activity"] == 0 {
		t.Fatal("expected activity events per batch")
	}

	// Replaying the same match again gives new event IDs.
	pub2 := &fakePub{}
	in2 := newTestIngester(pub2, &MemoryStatus{})
	in2.StatsBomb.BaseURL = in.StatsBomb.BaseURL
	in2.StatsBomb.Sleep = in.StatsBomb.Sleep
	in2.StatsBomb.Replay(context.Background(), messages.IngestCommand{ReplayID: "r2", MatchID: 3869685, Speed: 10}, 0)
	if byType(t, pub2.all(), "soccer.pass").EventID == pass.EventID {
		t.Fatal("event IDs must differ between replays")
	}
}

// Batching must not change the pace: total waits equal the match's game
// time divided by the speed (when no gap hits the cap).
func TestReplayPaceIsAccurate(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 600; i++ { // one event per game-second for 10 minutes
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"e%d","index":%d,"period":1,"timestamp":"00:%02d:%02d.000","minute":%d,"second":%d,"type":{"name":"Pass"},"team":{"name":"Argentina"},"possession_team":{"name":"Argentina"},"play_pattern":{"name":"Regular Play"}}`,
			i, i, i/60, i%60, i/60, i%60)
	}
	b.WriteString("]")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(b.String())) }))
	defer srv.Close()

	in := newTestIngester(&fakePub{}, &MemoryStatus{})
	in.StatsBomb.BaseURL = srv.URL
	var total time.Duration
	in.StatsBomb.Sleep = func(_ context.Context, d time.Duration) error { total += d; return nil }
	in.StatsBomb.Replay(context.Background(), messages.IngestCommand{ReplayID: "p", MatchID: 3869685, Speed: 60}, 0)
	// 599 game-seconds at 60× ≈ 9.98s of waiting.
	if want := 599 * time.Second / 60; total < want-10*time.Millisecond || total > want+10*time.Millisecond {
		t.Fatalf("total wait %v, want ≈%v", total, want)
	}
}

func TestReplayResumesFromPosition(t *testing.T) {
	pub, status := &fakePub{}, &MemoryStatus{}
	in := newTestIngester(pub, status)
	in.StatsBomb.BaseURL = sbServer(t).URL
	in.StatsBomb.Sleep = func(context.Context, time.Duration) error { return nil }
	in.StatsBomb.Replay(context.Background(), messages.IngestCommand{ReplayID: "r1", MatchID: 3869685, Speed: 60}, 4)
	evs := pub.all()
	if len(evs) != 3 {
		t.Fatalf("resume from 4: want 3 events, got %d", len(evs))
	}
	byType(t, evs, "soccer.half-start") // first event after the resume point
	if s := status.Snapshot("replay-r1")["score"].(map[string]int); s["Argentina"] != 1 {
		t.Fatalf("score should include goals before the resume point: %v", s)
	}
}

func cmdDelivery(cmd messages.IngestCommand) pubsubx.Delivery {
	b, _ := json.Marshal(cmd)
	return pubsubx.Delivery{Data: b}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestStopVersusShutdown(t *testing.T) {
	newIn := func(status *MemoryStatus) *Ingester {
		in := newTestIngester(&fakePub{}, status)
		in.StatsBomb.BaseURL = sbServer(t).URL
		// Block at the first wait until cancelled.
		in.StatsBomb.Sleep = func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
		return in
	}
	ctx := context.Background()

	// Admin stop: state "stopped".
	status := &MemoryStatus{}
	in := newIn(status)
	in.Start(ctx, false)
	in.HandleCommand(ctx, cmdDelivery(messages.IngestCommand{Action: "replay", ReplayID: "s1", MatchID: 3869685, Speed: 1}))
	waitFor(t, func() bool { return status.Snapshot("replay-s1")["state"] == "running" })

	// A redelivered replay command doesn't start a second replay.
	in.HandleCommand(ctx, cmdDelivery(messages.IngestCommand{Action: "replay", ReplayID: "s1", MatchID: 3869685, Speed: 1}))
	in.mu.Lock()
	n := len(in.replays)
	in.mu.Unlock()
	if n != 1 {
		t.Fatalf("duplicate command started %d replays", n)
	}

	in.HandleCommand(ctx, cmdDelivery(messages.IngestCommand{Action: "stop", ReplayID: "s1"}))
	waitFor(t, func() bool { return status.Snapshot("replay-s1")["state"] == "stopped" })

	// Shutdown: stays "running" with its position, and a new ingester resumes it.
	status = &MemoryStatus{}
	sctx, shutdown := context.WithCancel(ctx)
	in = newIn(status)
	in.Start(sctx, false)
	in.HandleCommand(sctx, cmdDelivery(messages.IngestCommand{Action: "replay", ReplayID: "s2", MatchID: 3869685, Speed: 1}))
	waitFor(t, func() bool { n, _ := status.Snapshot("replay-s2")["emitted"].(int); return n > 0 })
	shutdown()
	in.Wait()
	doc := status.Snapshot("replay-s2")
	if doc["state"] != "running" || doc["emitted"] == 0 {
		t.Fatalf("after shutdown: %v", doc)
	}

	pub := &fakePub{}
	in2 := newTestIngester(pub, status)
	in2.StatsBomb.BaseURL = in.StatsBomb.BaseURL
	in2.StatsBomb.Sleep = func(context.Context, time.Duration) error { return nil }
	in2.Start(ctx, false)
	waitFor(t, func() bool { return status.Snapshot("replay-s2")["state"] == "finished" })
	if got, total := len(pub.all())+doc["emitted"].(int), 7; got != total {
		t.Fatalf("resumed replay published %d after %v (want %d total)", len(pub.all()), doc["emitted"], total)
	}
}

func TestShutdownDuringDownloadStaysResumable(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)

	status := &MemoryStatus{}
	in := newTestIngester(&fakePub{}, status)
	in.StatsBomb.BaseURL = srv.URL
	ctx, shutdown := context.WithCancel(context.Background())
	in.Start(ctx, false)
	in.HandleCommand(ctx, cmdDelivery(messages.IngestCommand{Action: "replay", ReplayID: "d1", MatchID: 3869685, Speed: 10}))
	waitFor(t, func() bool { return status.Snapshot("replay-d1")["state"] == "running" })
	shutdown()
	in.Wait()
	if d := status.Snapshot("replay-d1"); d["state"] != "running" {
		t.Fatalf("shutdown mid-download should stay resumable: %v", d)
	}
}

func TestBadCommands(t *testing.T) {
	status := &MemoryStatus{}
	in := newTestIngester(&fakePub{}, status)
	in.Start(context.Background(), false)
	in.HandleCommand(context.Background(), cmdDelivery(messages.IngestCommand{Action: "replay", ReplayID: "x", MatchID: 1, Speed: 10}))
	if d := status.Snapshot("replay-x"); d["state"] != "failed" {
		t.Fatalf("unknown match: %v", d)
	}
	if err := in.HandleCommand(context.Background(), pubsubx.Delivery{Data: []byte("nope")}); err != nil {
		t.Fatal("malformed commands are dropped, not retried")
	}
}
