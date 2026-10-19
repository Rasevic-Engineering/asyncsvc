package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// NHL reads the public NHL web API (api-web.nhle.com): the day's scores and
// each game's play-by-play. A play's sortOrder is its sequence.
type NHL struct {
	BaseURL string // default https://api-web.nhle.com
	HTTP    *http.Client
	Every   time.Duration // default 10s
}

func (n *NHL) Name() string  { return "nhl" }
func (n *NHL) Label() string { return "NHL" }
func (n *NHL) PollEvery() time.Duration {
	if n.Every > 0 {
		return n.Every
	}
	return 10 * time.Second
}

type nhlTeam struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
	Score  int    `json:"score"`
	SOG    int    `json:"sog"`
	Name   struct {
		Default string `json:"default"`
	} `json:"commonName"`
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

func (n *NHL) url(path string) string {
	base := n.BaseURL
	if base == "" {
		base = "https://api-web.nhle.com"
	}
	return base + path
}

func nhlState(s string) string {
	switch s {
	case "LIVE", "CRIT":
		return Live
	case "FINAL", "OFF":
		return Final
	}
	return Scheduled
}

func nhlPeriod(num int, typ string) string {
	switch typ {
	case "OT":
		return "OT"
	case "SO":
		return "SO"
	}
	return fmt.Sprintf("P%d", num)
}

func (n *NHL) toGame(g nhlGame) Game {
	return Game{
		League: "nhl", ID: strconv.FormatInt(g.ID, 10), State: nhlState(g.GameState), StartTime: g.StartTimeUTC,
		Home: Team{Abbr: g.HomeTeam.Abbrev, Name: g.HomeTeam.Name.Default, Score: g.HomeTeam.Score},
		Away: Team{Abbr: g.AwayTeam.Abbrev, Name: g.AwayTeam.Name.Default, Score: g.AwayTeam.Score},
	}
}

func (n *NHL) Schedule(ctx context.Context) ([]Game, error) {
	var score struct {
		Games []nhlGame `json:"games"`
	}
	if err := getJSON(ctx, n.HTTP, n.url("/v1/score/now"), &score); err != nil {
		return nil, err
	}
	out := make([]Game, len(score.Games))
	for i, g := range score.Games {
		out[i] = n.toGame(g)
		if out[i].State == Final {
			out[i].StatusText = "Final"
		}
	}
	return out, nil
}

func (n *NHL) Plays(ctx context.Context, g Game) (Game, []Play, error) {
	var pbp nhlPlayByPlay
	if err := getJSON(ctx, n.HTTP, n.url("/v1/gamecenter/"+g.ID+"/play-by-play"), &pbp); err != nil {
		return g, nil, err
	}
	cur := n.toGame(pbp.nhlGame)
	period := nhlPeriod(pbp.PeriodDescriptor.Number, pbp.PeriodDescriptor.PeriodType)
	switch {
	case cur.State == Final:
		cur.StatusText = "Final"
	case pbp.Clock.InIntermission:
		cur.StatusText = period + " intermission"
	case cur.State == Live:
		cur.StatusText = period + " · " + pbp.Clock.TimeRemaining
	}
	cur.Extra = map[string]any{"away_sog": pbp.AwayTeam.SOG, "home_sog": pbp.HomeTeam.SOG}

	names := map[int64]string{}
	for _, r := range pbp.RosterSpots {
		names[r.PlayerID] = strings.TrimSpace(r.FirstName.Default + " " + r.LastName.Default)
	}
	teams := map[int64]string{pbp.HomeTeam.ID: pbp.HomeTeam.Abbrev, pbp.AwayTeam.ID: pbp.AwayTeam.Abbrev}

	plays := make([]Play, 0, len(pbp.Plays))
	for _, p := range pbp.Plays {
		team := teams[toInt64(p.Details["eventOwnerTeamId"], 0)]
		actor := nhlActor(p.Details, names)
		players := nhlPlayers(p.Details, names)
		plays = append(plays, Play{
			Seq:     p.SortOrder,
			Type:    p.TypeDescKey,
			Text:    nhlText(p.TypeDescKey, actor, team, p.Details),
			Team:    team,
			Actor:   actor,
			Period:  nhlPeriod(p.PeriodDescriptor.Number, p.PeriodDescriptor.PeriodType),
			Clock:   p.TimeRemaining,
			Scoring: p.TypeDescKey == "goal",
			Props: map[string]any{
				"time_in_period": p.TimeInPeriod,
				"time_remaining": p.TimeRemaining,
				"situation_code": p.SituationCode,
				"details":        p.Details,
				"players":        players,
			},
		})
	}
	return cur, plays, nil
}

// nhlActor picks the player a play is "about" (scorer, shooter, hitter, ...).
func nhlActor(details map[string]any, names map[int64]string) string {
	for _, k := range []string{"scoringPlayerId", "shootingPlayerId", "hittingPlayerId", "winningPlayerId",
		"committedByPlayerId", "blockingPlayerId", "playerId"} {
		if id := toInt64(details[k], 0); id != 0 {
			if name := names[id]; name != "" {
				return name
			}
			return fmt.Sprintf("player:%d", id)
		}
	}
	return ""
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

func nhlText(typ, actor, team string, d map[string]any) string {
	label := strings.ReplaceAll(typ, "-", " ")
	label = strings.ToUpper(label[:1]) + label[1:]
	parts := []string{label}
	if actor != "" {
		parts = append(parts, actor)
	}
	if team != "" {
		parts[len(parts)-1] += " (" + team + ")"
	}
	if st, ok := d["shotType"].(string); ok && st != "" {
		parts = append(parts, st)
	}
	if typ == "goal" {
		if a, h := toInt64(d["awayScore"], -1), toInt64(d["homeScore"], -1); a >= 0 && h >= 0 {
			parts = append(parts, fmt.Sprintf("%d–%d", a, h))
		}
	}
	if typ == "penalty" {
		if k, ok := d["descKey"].(string); ok {
			parts = append(parts, strings.ReplaceAll(k, "-", " "))
		}
	}
	return strings.Join(parts, " · ")
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
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err == nil {
			return n
		}
	}
	return def
}
