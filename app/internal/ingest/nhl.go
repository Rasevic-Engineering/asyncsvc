package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example/asyncsvc/internal/messages"
)

// NHL polls the public NHL web API (api-web.nhle.com): the day's schedule
// every ScheduleEvery, and each live game's play-by-play every PlaysEvery.
// Only plays not seen before (by sortOrder, kept in the status store so it
// survives restarts) are published.
type NHL struct {
	BaseURL       string // default https://api-web.nhle.com
	HTTP          *http.Client
	ScheduleEvery time.Duration // default 60s
	PlaysEvery    time.Duration // default 10s

	sink   *sink
	status StatusStore

	mu       sync.Mutex
	tracking map[int64]context.CancelFunc
}

type nhlTeam struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
	Score  int    `json:"score"`
	SOG    int    `json:"sog"`
}

type nhlGame struct {
	ID           int64   `json:"id"`
	GameState    string  `json:"gameState"` // FUT, PRE, LIVE, CRIT, FINAL, OFF
	StartTimeUTC string  `json:"startTimeUTC"`
	AwayTeam     nhlTeam `json:"awayTeam"`
	HomeTeam     nhlTeam `json:"homeTeam"`
}

type nhlPlayByPlay struct {
	nhlGame
	PeriodDescriptor struct {
		Number     int    `json:"number"`
		PeriodType string `json:"periodType"`
	} `json:"periodDescriptor"`
	Clock struct {
		TimeRemaining  string `json:"timeRemaining"`
		InIntermission bool   `json:"inIntermission"`
	} `json:"clock"`
	Plays []struct {
		EventID          int64 `json:"eventId"`
		SortOrder        int64 `json:"sortOrder"`
		PeriodDescriptor struct {
			Number     int    `json:"number"`
			PeriodType string `json:"periodType"`
		} `json:"periodDescriptor"`
		TimeInPeriod  string         `json:"timeInPeriod"`
		TimeRemaining string         `json:"timeRemaining"`
		SituationCode string         `json:"situationCode"`
		TypeDescKey   string         `json:"typeDescKey"`
		Details       map[string]any `json:"details"`
	} `json:"plays"`
	RosterSpots []struct {
		TeamID    int64 `json:"teamId"`
		PlayerID  int64 `json:"playerId"`
		FirstName struct {
			Default string `json:"default"`
		} `json:"firstName"`
		LastName struct {
			Default string `json:"default"`
		} `json:"lastName"`
	} `json:"rosterSpots"`
}

func isLive(state string) bool { return state == "LIVE" || state == "CRIT" }
func isOver(state string) bool { return state == "FINAL" || state == "OFF" }

func (n *NHL) get(ctx context.Context, path string, v any) error {
	base := n.BaseURL
	if base == "" {
		base = "https://api-web.nhle.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "asyncsvc-ingester (personal project)")
	hc := n.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second} // follows /now -> /<date> redirects
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Run polls until ctx is cancelled.
func (n *NHL) Run(ctx context.Context) {
	every := n.ScheduleEvery
	if every == 0 {
		every = time.Minute
	}
	n.tracking = map[int64]context.CancelFunc{}
	var wg sync.WaitGroup
	defer wg.Wait()

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := n.checkSchedule(ctx, &wg); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "nhl schedule", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *NHL) checkSchedule(ctx context.Context, wg *sync.WaitGroup) error {
	var score struct {
		Games []nhlGame `json:"games"`
	}
	if err := n.get(ctx, "/v1/score/now", &score); err != nil {
		return err
	}
	for _, g := range score.Games {
		id := g.ID
		n.mu.Lock()
		_, tracked := n.tracking[id]
		n.mu.Unlock()

		switch {
		case isLive(g.GameState) && !tracked:
			gctx, cancel := context.WithCancel(ctx)
			n.mu.Lock()
			n.tracking[id] = cancel
			n.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					n.mu.Lock()
					delete(n.tracking, id)
					n.mu.Unlock()
				}()
				n.trackGame(gctx, id)
			}()
		case !isLive(g.GameState) && !tracked:
			// Scheduled or finished games: keep their card current.
			if err := n.status.Merge(ctx, nhlDocID(id), map[string]any{
				"kind": "nhl", "game_id": id, "state": g.GameState, "start_time": g.StartTimeUTC,
				"away": g.AwayTeam.Abbrev, "home": g.HomeTeam.Abbrev,
				"away_score": g.AwayTeam.Score, "home_score": g.HomeTeam.Score,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func nhlDocID(id int64) string { return fmt.Sprintf("nhl-%d", id) }

// trackGame polls one game's play-by-play until it's over.
func (n *NHL) trackGame(ctx context.Context, id int64) {
	every := n.PlaysEvery
	if every == 0 {
		every = 10 * time.Second
	}
	log := slog.With("nhl_game", id)
	log.InfoContext(ctx, "tracking NHL game")

	var lastSort, ingested int64 = -1, 0
	if doc, ok, err := n.status.Get(ctx, nhlDocID(id)); err == nil && ok {
		lastSort = toInt64(doc["last_sort_order"], -1)
		ingested = toInt64(doc["plays_ingested"], 0)
	}

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		over, err := n.pollPlays(ctx, id, &lastSort, &ingested)
		if err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "nhl play-by-play", "err", err)
		}
		if over {
			log.InfoContext(ctx, "NHL game over", "plays", ingested)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *NHL) pollPlays(ctx context.Context, id int64, lastSort, ingested *int64) (over bool, err error) {
	var pbp nhlPlayByPlay
	if err := n.get(ctx, fmt.Sprintf("/v1/gamecenter/%d/play-by-play", id), &pbp); err != nil {
		return false, err
	}
	names := map[int64]string{}
	for _, r := range pbp.RosterSpots {
		names[r.PlayerID] = strings.TrimSpace(r.FirstName.Default + " " + r.LastName.Default)
	}
	teams := map[int64]string{pbp.HomeTeam.ID: pbp.HomeTeam.Abbrev, pbp.AwayTeam.ID: pbp.AwayTeam.Abbrev}

	sort.Slice(pbp.Plays, func(i, j int) bool { return pbp.Plays[i].SortOrder < pbp.Plays[j].SortOrder })
	now := time.Now()
	gameID := fmt.Sprintf("nhl:%d", id)
	var events []messages.Event
	newLast := *lastSort
	for _, p := range pbp.Plays {
		if p.SortOrder <= *lastSort {
			continue
		}
		newLast = max(newLast, p.SortOrder)
		team := teams[toInt64(p.Details["eventOwnerTeamId"], 0)]
		events = append(events, messages.Event{
			EventID:    eventID("nhl", strconv.FormatInt(id, 10), strconv.FormatInt(p.EventID, 10)),
			EventType:  "nhl." + p.TypeDescKey,
			UserID:     nhlActor(p.Details, names, team),
			OccurredAt: now.UnixMicro(),
			ReceivedAt: now.UnixMicro(),
			Source:     "nhl",
			GameID:     gameID,
			Team:       team,
			Properties: mustJSON(map[string]any{
				"game":           fmt.Sprintf("%s @ %s", pbp.AwayTeam.Abbrev, pbp.HomeTeam.Abbrev),
				"period":         p.PeriodDescriptor.Number,
				"period_type":    p.PeriodDescriptor.PeriodType,
				"time_in_period": p.TimeInPeriod,
				"time_remaining": p.TimeRemaining,
				"situation_code": p.SituationCode,
				"details":        p.Details,
				"players":        nhlPlayers(p.Details, names),
			}),
		})
	}

	label := fmt.Sprintf("NHL %s @ %s", pbp.AwayTeam.Abbrev, pbp.HomeTeam.Abbrev)
	if err := n.sink.publish(ctx, label, events); err != nil {
		return false, err // lastSort not advanced: retried next poll
	}
	*lastSort = newLast
	*ingested += int64(len(events))

	err = n.status.Merge(ctx, nhlDocID(id), map[string]any{
		"kind": "nhl", "game_id": id, "state": pbp.GameState, "start_time": pbp.StartTimeUTC,
		"away": pbp.AwayTeam.Abbrev, "home": pbp.HomeTeam.Abbrev,
		"away_score": pbp.AwayTeam.Score, "home_score": pbp.HomeTeam.Score,
		"away_sog": pbp.AwayTeam.SOG, "home_sog": pbp.HomeTeam.SOG,
		"period": pbp.PeriodDescriptor.Number, "period_type": pbp.PeriodDescriptor.PeriodType,
		"clock": pbp.Clock.TimeRemaining, "intermission": pbp.Clock.InIntermission,
		"plays_ingested": *ingested, "last_sort_order": *lastSort,
	})
	return isOver(pbp.GameState), err
}

// nhlActor picks the player a play is "about" (scorer, shooter, hitter, ...),
// falling back to the team.
func nhlActor(details map[string]any, names map[int64]string, team string) string {
	for _, k := range []string{"scoringPlayerId", "shootingPlayerId", "hittingPlayerId", "winningPlayerId",
		"committedByPlayerId", "blockingPlayerId", "playerId"} {
		if id := toInt64(details[k], 0); id != 0 {
			if name := names[id]; name != "" {
				return name
			}
			return fmt.Sprintf("player:%d", id)
		}
	}
	if team != "" {
		return "team:" + team
	}
	return "game"
}

// nhlPlayers resolves every *PlayerId in a play's details to a name.
func nhlPlayers(details map[string]any, names map[int64]string) map[string]string {
	out := map[string]string{}
	for k, v := range details {
		if strings.HasSuffix(k, "PlayerId") || strings.HasSuffix(k, "InNetId") {
			if name := names[toInt64(v, 0)]; name != "" {
				out[strings.TrimSuffix(strings.TrimSuffix(k, "PlayerId"), "Id")] = name
			}
		}
	}
	return out
}

func toInt64(v any, def int64) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		n, err := x.Int64()
		if err == nil {
			return n
		}
	}
	return def
}
