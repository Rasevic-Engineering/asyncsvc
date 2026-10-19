package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example/asyncsvc/internal/messages"
)

// League adapts one live data source (NHL, MLB, NBA, NFL, soccer) to the
// shared Tracker, which does scheduling, de-duplication, publishing and
// status. A league only knows how to read its schedule and its plays.
type League interface {
	// Name is the event source and type prefix, e.g. "nhl" → nhl.goal.
	Name() string
	// Label is shown in the UI, e.g. "NHL".
	Label() string
	// Schedule lists today's games.
	Schedule(ctx context.Context) ([]Game, error)
	// Plays returns the game's current state and all its plays so far.
	Plays(ctx context.Context, g Game) (Game, []Play, error)
	// PollEvery is how often a live game is polled.
	PollEvery() time.Duration
}

// Game states, normalized across leagues.
const (
	Scheduled = "scheduled"
	Live      = "live"
	Final     = "final"
)

type Team struct {
	Abbr  string `json:"abbr"`
	Name  string `json:"name"`
	Score int    `json:"score"`
}

type Game struct {
	League      string
	ID          string // league's own game ID
	State       string // Scheduled, Live, Final
	StatusText  string // e.g. "P2 · 01:10", "Bot 7th", "Q3 · 4:12", "67'"
	StartTime   string // RFC 3339
	Home, Away  Team
	Competition string // soccer league, playoff round, ...
	// Extra is league-specific status for the UI (e.g. shots on goal, count).
	Extra map[string]any
}

// Play is one event in a game. Seq is unique within the game and increases
// over time, so "new" means Seq > the highest Seq already published.
type Play struct {
	Seq     int64
	Type    string // slug, e.g. "goal", "pitch", "pass-reception"
	Text    string // human-readable, for the UI
	Team    string // abbreviation
	Actor   string // player, or "team:<abbr>"
	Period  string // "P2", "Top 3rd", "Q4", "2H"
	Clock   string
	Scoring bool
	Props   map[string]any
}

func gameDocID(league, id string) string { return "game-" + league + "-" + id }

// Tracker polls each league's schedule and its live games.
type Tracker struct {
	Leagues       []League
	ScheduleEvery time.Duration // default 60s

	sink   *sink
	status StatusStore

	mu       sync.Mutex
	tracking map[string]bool
}

// Run polls until ctx is cancelled.
func (t *Tracker) Run(ctx context.Context) {
	every := t.ScheduleEvery
	if every == 0 {
		every = time.Minute
	}
	t.mu.Lock()
	t.tracking = map[string]bool{}
	t.mu.Unlock()

	var wg sync.WaitGroup
	defer wg.Wait()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		for _, l := range t.Leagues {
			if err := t.checkSchedule(ctx, l, &wg); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "schedule", "league", l.Name(), "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (t *Tracker) checkSchedule(ctx context.Context, l League, wg *sync.WaitGroup) error {
	games, err := l.Schedule(ctx)
	if err != nil {
		return err
	}
	for _, g := range games {
		key := gameDocID(l.Name(), g.ID)
		t.mu.Lock()
		tracked := t.tracking[key]
		if g.State == Live && !tracked {
			t.tracking[key] = true
		}
		t.mu.Unlock()

		switch {
		case g.State == Live && !tracked:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					t.mu.Lock()
					delete(t.tracking, key)
					t.mu.Unlock()
				}()
				t.trackGame(ctx, l, g)
			}()
		case !tracked:
			// Scheduled or finished: keep the scoreboard card current.
			if err := t.status.Merge(ctx, key, gameStatus(l, g, nil)); err != nil {
				return err
			}
		}
	}
	return nil
}

func gameStatus(l League, g Game, extra map[string]any) map[string]any {
	doc := map[string]any{
		"kind": "game", "league": l.Name(), "league_label": l.Label(), "game_id": g.ID,
		"state": g.State, "status_text": g.StatusText, "start_time": g.StartTime,
		"home": map[string]any{"abbr": g.Home.Abbr, "name": g.Home.Name, "score": g.Home.Score},
		"away": map[string]any{"abbr": g.Away.Abbr, "name": g.Away.Name, "score": g.Away.Score},
	}
	if g.Competition != "" {
		doc["competition"] = g.Competition
	}
	if len(g.Extra) > 0 {
		doc["extra"] = g.Extra
	}
	for k, v := range extra {
		doc[k] = v
	}
	return doc
}

// trackGame polls one live game until it's final.
func (t *Tracker) trackGame(ctx context.Context, l League, g Game) {
	key := gameDocID(l.Name(), g.ID)
	log := slog.With("league", l.Name(), "game", g.ID)
	log.InfoContext(ctx, "tracking game", "away", g.Away.Abbr, "home", g.Home.Abbr)

	var lastSeq, ingested int64 = -1, 0
	if doc, ok, err := t.status.Get(ctx, key); err == nil && ok {
		lastSeq = toInt64(doc["last_seq"], -1)
		ingested = toInt64(doc["plays_ingested"], 0)
	}

	tick := time.NewTicker(l.PollEvery())
	defer tick.Stop()
	for {
		over, err := t.poll(ctx, l, g, &lastSeq, &ingested)
		if err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "poll", "err", err)
		}
		if over {
			log.InfoContext(ctx, "game over", "plays", ingested)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (t *Tracker) poll(ctx context.Context, l League, g Game, lastSeq, ingested *int64) (over bool, err error) {
	cur, plays, err := l.Plays(ctx, g)
	if err != nil {
		return false, err
	}
	sort.SliceStable(plays, func(i, j int) bool { return plays[i].Seq < plays[j].Seq })

	now := time.Now()
	gameID := l.Name() + ":" + g.ID
	matchup := fmt.Sprintf("%s @ %s", cur.Away.Abbr, cur.Home.Abbr)
	var events []messages.Event
	newLast := *lastSeq
	for _, p := range plays {
		if p.Seq <= *lastSeq {
			continue
		}
		newLast = max(newLast, p.Seq)
		props := map[string]any{"game": matchup, "period": p.Period, "clock": p.Clock, "text": p.Text, "seq": p.Seq}
		if p.Scoring {
			props["scoring"] = true
		}
		if cur.Competition != "" {
			props["competition"] = cur.Competition
		}
		for k, v := range p.Props {
			props[k] = v
		}
		actor := p.Actor
		if actor == "" {
			actor = "game"
			if p.Team != "" {
				actor = "team:" + p.Team
			}
		}
		// Plays from one poll share the poll time; a microsecond per play
		// keeps them in game order wherever they're sorted by time.
		at := now.Add(time.Duration(len(events)) * time.Microsecond)
		events = append(events, messages.Event{
			EventID:    eventID(l.Name(), g.ID, fmt.Sprint(p.Seq)),
			EventType:  l.Name() + "." + p.Type,
			UserID:     actor,
			OccurredAt: at.UnixMicro(),
			ReceivedAt: now.UnixMicro(),
			Source:     l.Name(),
			GameID:     gameID,
			Team:       p.Team,
			Properties: mustJSON(props),
		})
	}

	// Every live play is shown in its sport's stream in the UI.
	ui := make([]bool, len(events))
	for i := range ui {
		ui[i] = true
	}
	label := fmt.Sprintf("%s %s", l.Label(), matchup)
	if err := t.sink.publish(ctx, label, events, ui); err != nil {
		return false, err // position not advanced: retried next poll
	}
	*lastSeq = newLast
	*ingested += int64(len(events))

	err = t.status.Merge(ctx, gameDocID(l.Name(), g.ID), gameStatus(l, cur, map[string]any{
		"plays_ingested": *ingested, "last_seq": *lastSeq,
	}))
	return cur.State == Final, err
}

// ---- shared helpers for league adapters -----------------------------------

const userAgent = "asyncsvc-ingester (personal project)"

func getJSON(ctx context.Context, hc *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second} // follows redirects
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// typeSlug turns "Free Throw - 3 of 3" or "Pass Reception" into a stable
// event type ("free-throw", "pass-reception").
func typeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, " - "); i > 0 {
		s = s[:i]
	}
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// Leagues parses a comma-separated list ("nhl,mlb,nba,nfl,soccer"); empty
// or "none" disables live polling (replays still work).
func Leagues(list string) ([]League, error) {
	var out []League
	for _, name := range strings.Split(list, ",") {
		switch strings.TrimSpace(strings.ToLower(name)) {
		case "", "none":
		case "nhl":
			out = append(out, &NHL{})
		case "mlb":
			out = append(out, &MLB{})
		case "nba":
			out = append(out, NewNBA())
		case "nfl":
			out = append(out, NewNFL())
		case "soccer":
			out = append(out, NewSoccer())
		default:
			return nil, fmt.Errorf("unknown league %q (known: nhl, mlb, nba, nfl, soccer)", name)
		}
	}
	return out, nil
}
