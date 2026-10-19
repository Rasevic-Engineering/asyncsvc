package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/example/asyncsvc/internal/messages"
)

// Match is a replayable StatsBomb open-data match.
type Match struct {
	ID          int    `json:"match_id"`
	Label       string `json:"label"`
	Competition string `json:"competition"`
	Date        string `json:"date"`
}

// Catalog is the set of matches admins can replay (StatsBomb open data;
// IDs verified against data/matches/<competition>/<season>.json).
var Catalog = []Match{
	{3869685, "Argentina 3–3 France (pens 4–2)", "FIFA World Cup 2022 · Final", "2022-12-18"},
	{3869552, "France 2–0 Morocco", "FIFA World Cup 2022 · Semi-final", "2022-12-14"},
	{8658, "France 4–2 Croatia", "FIFA World Cup 2018 · Final", "2018-07-15"},
	{3943043, "Spain 2–1 England", "UEFA Euro 2024 · Final", "2024-07-14"},
	{3795506, "Italy 1–1 England (pens 3–2)", "UEFA Euro 2020 · Final", "2021-07-11"},
	{22912, "Tottenham 0–2 Liverpool", "Champions League 2018/19 · Final", "2019-06-01"},
	{3906390, "Spain 1–0 England", "FIFA Women's World Cup 2023 · Final", "2023-08-20"},
}

// Speeds are the allowed replay speeds (1× is real time).
var Speeds = []int{1, 5, 10, 30, 60}

func CatalogMatch(id int) (Match, bool) {
	for _, m := range Catalog {
		if m.ID == id {
			return m, true
		}
	}
	return Match{}, false
}

func ValidSpeed(s int) bool {
	for _, v := range Speeds {
		if v == s {
			return true
		}
	}
	return false
}

// StatsBomb replays matches from the StatsBomb open-data repository.
type StatsBomb struct {
	BaseURL string // default https://raw.githubusercontent.com/statsbomb/open-data/master/data
	HTTP    *http.Client
	// Sleep waits for d or until ctx is done (tests replace it).
	Sleep func(ctx context.Context, d time.Duration) error

	sink   *sink
	status StatusStore
}

type sbEvent struct {
	ID        string `json:"id"`
	Index     int    `json:"index"`
	Period    int    `json:"period"`
	Timestamp string `json:"timestamp"` // "00:04:40.798", relative to the period start
	Minute    int    `json:"minute"`
	Second    int    `json:"second"`
	Type      struct {
		Name string `json:"name"`
	} `json:"type"`
	Team struct {
		Name string `json:"name"`
	} `json:"team"`
	Player *struct {
		Name string `json:"name"`
	} `json:"player"`
	PossessionTeam struct {
		Name string `json:"name"`
	} `json:"possession_team"`
	PlayPattern struct {
		Name string `json:"name"`
	} `json:"play_pattern"`
	Location []float64 `json:"location"`
	Shot     *struct {
		XG      float64 `json:"statsbomb_xg"`
		Outcome struct {
			Name string `json:"name"`
		} `json:"outcome"`
		BodyPart struct {
			Name string `json:"name"`
		} `json:"body_part"`
		Type struct {
			Name string `json:"name"`
		} `json:"type"`
		EndLocation []float64 `json:"end_location"`
	} `json:"shot"`
	Pass *struct {
		Length    float64 `json:"length"`
		Recipient *struct {
			Name string `json:"name"`
		} `json:"recipient"`
		Outcome *struct {
			Name string `json:"name"`
		} `json:"outcome"`
		EndLocation []float64 `json:"end_location"`
	} `json:"pass"`
}

func (e sbEvent) offset() time.Duration {
	// "HH:MM:SS.mmm"
	var h, m int
	var s float64
	fmt.Sscanf(e.Timestamp, "%d:%d:%f", &h, &m, &s)
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s*float64(time.Second))
}

func (s *StatsBomb) fetch(ctx context.Context, matchID int) ([]sbEvent, error) {
	base := s.BaseURL
	if base == "" {
		base = "https://raw.githubusercontent.com/statsbomb/open-data/master/data"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/events/%d.json", base, matchID), nil)
	if err != nil {
		return nil, err
	}
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("statsbomb match %d: %s", matchID, resp.Status)
	}
	var events []sbEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Period != events[j].Period {
			return events[i].Period < events[j].Period
		}
		return events[i].Index < events[j].Index
	})
	return events, nil
}

func replayDocID(id string) string { return "replay-" + id }

// ErrStopped is the cancellation cause for an admin's Stop, as opposed to
// the ingester shutting down (after which the replay resumes).
var ErrStopped = errors.New("replay stopped")

const (
	// Never wait longer than this (real time) between events, e.g. across
	// long stoppages; and pause this long between periods.
	maxGap      = 5 * time.Second
	periodPause = 2 * time.Second
	// Events closer together than this are published as one batch.
	batchWindow = 250 * time.Millisecond
	// Write progress to the status store at most this often.
	statusEvery = 2 * time.Second
)

// Replay streams a match's events, starting after `from` already-published
// events (to resume after a restart). It returns when the match is over or
// ctx is cancelled (Stop).
func (s *StatsBomb) Replay(ctx context.Context, cmd messages.IngestCommand, from int) error {
	match, _ := CatalogMatch(cmd.MatchID)
	doc := replayDocID(cmd.ReplayID)
	log := slog.With("replay_id", cmd.ReplayID, "match_id", cmd.MatchID, "speed", cmd.Speed)

	// Recorded as running before anything else, so a shutdown at any point
	// (even mid-download) leaves it resumable.
	if err := s.status.Merge(ctx, doc, map[string]any{
		"kind": "statsbomb", "replay_id": cmd.ReplayID, "match_id": cmd.MatchID, "label": match.Label,
		"competition": match.Competition, "speed": cmd.Speed, "requested_by": cmd.RequestedBy,
		"state": "running", "emitted": from,
	}); err != nil {
		return err
	}
	fail := func(err error) error {
		if ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrStopped) {
			return err // shutting down: stays "running", resumes later
		}
		state := "failed"
		if errors.Is(context.Cause(ctx), ErrStopped) {
			state = "stopped"
		}
		_ = s.status.Merge(context.WithoutCancel(ctx), doc, map[string]any{"state": state, "error": err.Error()})
		return err
	}

	events, err := s.fetch(ctx, cmd.MatchID)
	if err != nil {
		return fail(err)
	}
	if err := s.status.Merge(ctx, doc, map[string]any{"total": len(events)}); err != nil {
		return fail(err)
	}
	log.InfoContext(ctx, "replay started", "events", len(events), "from", from)

	sleep := s.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}

	score := map[string]int{}
	for _, e := range events[:min(from, len(events))] {
		countGoal(score, e)
	}
	gameID := fmt.Sprintf("statsbomb:%d", cmd.MatchID)
	label := fmt.Sprintf("Replay %s ×%d", match.Label, cmd.Speed)
	lastStatus := time.Time{}

	var batch []messages.Event
	emitted := from
	flush := func(i int, e sbEvent, final bool) error {
		if err := s.sink.publish(ctx, label, batch); err != nil {
			return err
		}
		batch = batch[:0]
		emitted = i + 1
		if final || time.Since(lastStatus) >= statusEvery {
			lastStatus = time.Now()
			return s.status.Merge(ctx, doc, map[string]any{
				"emitted": i + 1, "period": e.Period, "minute": e.Minute, "second": e.Second, "score": score,
			})
		}
		return nil
	}

	// owed is the replay-time wait accumulated while batching events that
	// are close together; it's slept in one go, so batching never speeds up
	// the replay.
	var owed time.Duration
	for i := from; i < len(events); i++ {
		e := events[i]
		batch = append(batch, s.toEvent(cmd, gameID, match, e))
		countGoal(score, e)

		// The gap to the next event, scaled by speed.
		if i+1 < len(events) {
			next := events[i+1]
			if next.Period != e.Period {
				owed += periodPause
			} else {
				owed += min((next.offset()-e.offset())/time.Duration(cmd.Speed), maxGap)
			}
		}
		if owed < batchWindow && i+1 < len(events) && len(batch) < 200 {
			continue // keep batching
		}
		if err := flush(i, e, i+1 == len(events)); err != nil {
			if errors.Is(err, context.Canceled) {
				break
			}
			return fail(err)
		}
		if owed > 0 {
			if err := sleep(ctx, owed); err != nil {
				break
			}
			owed = 0
		}
	}

	final := map[string]any{"score": score, "emitted": emitted}
	switch {
	case errors.Is(context.Cause(ctx), ErrStopped):
		final["state"] = "stopped"
		log.InfoContext(ctx, "replay stopped")
	case ctx.Err() != nil:
		// Shutting down: stay "running" so the next instance resumes from
		// the saved position.
		log.InfoContext(ctx, "replay interrupted; will resume", "emitted", emitted)
	default:
		final["state"] = "finished"
		log.InfoContext(ctx, "replay finished")
	}
	return s.status.Merge(context.WithoutCancel(ctx), doc, final)
}

// countGoal keeps the running score (regulation and extra time; shootout
// penalties, period 5, aren't goals in the score).
func countGoal(score map[string]int, e sbEvent) {
	if e.Shot != nil && e.Shot.Outcome.Name == "Goal" && e.Period < 5 {
		score[e.Team.Name]++
	}
	if e.Type.Name == "Own Goal For" {
		score[e.Team.Name]++
	}
}

func (s *StatsBomb) toEvent(cmd messages.IngestCommand, gameID string, m Match, e sbEvent) messages.Event {
	now := time.Now()
	player := "team:" + e.Team.Name
	if e.Player != nil && e.Player.Name != "" {
		player = e.Player.Name
	}
	props := map[string]any{
		"match":           m.Label,
		"competition":     m.Competition,
		"match_date":      m.Date,
		"replay_id":       cmd.ReplayID,
		"speed":           cmd.Speed,
		"period":          e.Period,
		"minute":          e.Minute,
		"second":          e.Second,
		"possession_team": e.PossessionTeam.Name,
		"play_pattern":    e.PlayPattern.Name,
	}
	if len(e.Location) == 2 {
		props["x"], props["y"] = e.Location[0], e.Location[1]
	}
	if e.Shot != nil {
		props["xg"] = e.Shot.XG
		props["outcome"] = e.Shot.Outcome.Name
		props["body_part"] = e.Shot.BodyPart.Name
		props["shot_type"] = e.Shot.Type.Name
	}
	if e.Pass != nil {
		props["length"] = e.Pass.Length
		if e.Pass.Recipient != nil {
			props["recipient"] = e.Pass.Recipient.Name
		}
		props["outcome"] = "Complete"
		if e.Pass.Outcome != nil {
			props["outcome"] = e.Pass.Outcome.Name
		}
	}
	return messages.Event{
		// Unique per replay, so the same match can be replayed many times.
		EventID:    eventID("statsbomb", cmd.ReplayID, e.ID),
		EventType:  "soccer." + slug(e.Type.Name),
		UserID:     player,
		OccurredAt: now.UnixMicro(),
		ReceivedAt: now.UnixMicro(),
		Source:     "statsbomb",
		GameID:     gameID,
		Team:       e.Team.Name,
		Properties: mustJSON(props),
	}
}

// slug turns "Ball Receipt*" into "ball-receipt".
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.TrimRight(s, "*")))
	return strings.Join(strings.Fields(s), "-")
}
