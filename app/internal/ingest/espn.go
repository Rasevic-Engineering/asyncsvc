package ingest

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ESPN reads ESPN's public site API (site.api.espn.com) for one sport: the
// scoreboard of each league path, and each game's summary (play-by-play for
// basketball and football, commentary for soccer). It's undocumented and
// may change; fine for a personal project.
type ESPN struct {
	Sport     string   // "nba", "nfl", "soccer": event source and type prefix
	SportName string   // "NBA", "NFL", "Soccer"
	Paths     []string // league paths, e.g. "basketball/nba", "soccer/eng.1"
	BaseURL   string   // default https://site.api.espn.com/apis/site/v2/sports
	HTTP      *http.Client
	Every     time.Duration

	mu    sync.Mutex
	paths map[string]string // game ID → league path
}

// NewNBA, NewNFL and NewSoccer return the ESPN adapters the ingester uses.
func NewNBA() *ESPN {
	return &ESPN{Sport: "nba", SportName: "NBA", Paths: []string{"basketball/nba"}, Every: 10 * time.Second}
}

func NewNFL() *ESPN {
	return &ESPN{Sport: "nfl", SportName: "NFL", Paths: []string{"football/nfl"}, Every: 15 * time.Second}
}

// SoccerLeagues are the competitions followed live.
var SoccerLeagues = []string{
	"soccer/eng.1", "soccer/esp.1", "soccer/ita.1", "soccer/ger.1", "soccer/fra.1",
	"soccer/usa.1", "soccer/uefa.champions", "soccer/uefa.europa",
}

func NewSoccer() *ESPN {
	return &ESPN{Sport: "soccer", SportName: "Soccer", Paths: SoccerLeagues, Every: 30 * time.Second}
}

func (e *ESPN) Name() string  { return e.Sport }
func (e *ESPN) Label() string { return e.SportName }
func (e *ESPN) PollEvery() time.Duration {
	if e.Every > 0 {
		return e.Every
	}
	return 15 * time.Second
}

func (e *ESPN) url(path, rest string) string {
	base := e.BaseURL
	if base == "" {
		base = "https://site.api.espn.com/apis/site/v2/sports"
	}
	return base + "/" + path + "/" + rest
}

type espnStatus struct {
	Period       int    `json:"period"`
	DisplayClock string `json:"displayClock"`
	Type         struct {
		State       string `json:"state"` // pre, in, post
		ShortDetail string `json:"shortDetail"`
		Detail      string `json:"detail"`
	} `json:"type"`
}

type espnCompetitor struct {
	HomeAway string `json:"homeAway"`
	Score    string `json:"score"`
	Team     struct {
		ID           string `json:"id"`
		Abbreviation string `json:"abbreviation"`
		DisplayName  string `json:"displayName"`
		Name         string `json:"name"`
	} `json:"team"`
}

type espnCompetition struct {
	Date        string           `json:"date"`
	Status      espnStatus       `json:"status"`
	Competitors []espnCompetitor `json:"competitors"`
}

func espnState(s string) string {
	switch s {
	case "in":
		return Live
	case "post":
		return Final
	}
	return Scheduled
}

func (e *ESPN) toGame(id, league string, c espnCompetition, status espnStatus) Game {
	g := Game{
		League: e.Sport, ID: id, State: espnState(status.Type.State), StatusText: status.Type.ShortDetail,
		StartTime: c.Date, Competition: league,
	}
	for _, t := range c.Competitors {
		score, _ := strconv.Atoi(t.Score)
		team := Team{Abbr: t.Team.Abbreviation, Name: t.Team.DisplayName, Score: score}
		if t.HomeAway == "home" {
			g.Home = team
		} else {
			g.Away = team
		}
	}
	return g
}

func (e *ESPN) Schedule(ctx context.Context) ([]Game, error) {
	var out []Game
	var firstErr error
	for _, path := range e.Paths {
		var sb struct {
			Leagues []struct {
				Name string `json:"name"`
			} `json:"leagues"`
			Events []struct {
				ID           string            `json:"id"`
				Status       espnStatus        `json:"status"`
				Competitions []espnCompetition `json:"competitions"`
			} `json:"events"`
		}
		if err := getJSON(ctx, e.HTTP, e.url(path, "scoreboard"), &sb); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue // one league failing shouldn't hide the others
		}
		league := ""
		if len(sb.Leagues) > 0 && len(e.Paths) > 1 {
			league = sb.Leagues[0].Name
		}
		for _, ev := range sb.Events {
			if len(ev.Competitions) == 0 {
				continue
			}
			e.mu.Lock()
			if e.paths == nil {
				e.paths = map[string]string{}
			}
			e.paths[ev.ID] = path
			e.mu.Unlock()
			out = append(out, e.toGame(ev.ID, league, ev.Competitions[0], ev.Status))
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

type espnTeamRef struct {
	ID           string `json:"id"`
	Abbreviation string `json:"abbreviation"`
	DisplayName  string `json:"displayName"`
}

type espnPlay struct {
	ID             string `json:"id"`
	SequenceNumber string `json:"sequenceNumber"`
	Text           string `json:"text"`
	Type           struct {
		Text string `json:"text"`
	} `json:"type"`
	Period struct {
		Number int `json:"number"`
	} `json:"period"`
	Clock struct {
		DisplayValue string `json:"displayValue"`
	} `json:"clock"`
	ScoringPlay bool         `json:"scoringPlay"`
	ScoreValue  int          `json:"scoreValue"`
	AwayScore   int          `json:"awayScore"`
	HomeScore   int          `json:"homeScore"`
	Team        *espnTeamRef `json:"team"`
	Coordinate  *struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"coordinate"`
	Participants []struct {
		Athlete struct {
			DisplayName string `json:"displayName"`
		} `json:"athlete"`
	} `json:"participants"`
	// Football only.
	Start *struct {
		DownDistanceText string `json:"downDistanceText"`
		YardLine         int    `json:"yardLine"`
	} `json:"start"`
	StatYardage int  `json:"statYardage"`
	IsTurnover  bool `json:"isTurnover"`
	IsPenalty   bool `json:"isPenalty"`
}

type espnSummary struct {
	Header struct {
		League struct {
			Name string `json:"name"`
		} `json:"league"`
		Competitions []espnCompetition `json:"competitions"`
	} `json:"header"`
	// Basketball.
	Plays []espnPlay `json:"plays"`
	// Football: plays are grouped into drives.
	Drives struct {
		Previous []espnDrive `json:"previous"`
		Current  *espnDrive  `json:"current"`
	} `json:"drives"`
	// Soccer.
	Commentary []struct {
		Sequence int64 `json:"sequence"`
		Time     struct {
			DisplayValue string `json:"displayValue"`
		} `json:"time"`
		Text string `json:"text"`
		Play *struct {
			ID   string `json:"id"`
			Type struct {
				Text string `json:"text"`
				Type string `json:"type"`
			} `json:"type"`
			Period struct {
				Number int `json:"number"`
			} `json:"period"`
			ScoringPlay bool `json:"scoringPlay"`
			Team        *struct {
				DisplayName string `json:"displayName"`
			} `json:"team"`
			Participants []struct {
				Athlete struct {
					DisplayName string `json:"displayName"`
				} `json:"athlete"`
			} `json:"participants"`
		} `json:"play"`
	} `json:"commentary"`
}

type espnDrive struct {
	Team struct {
		Abbreviation string `json:"abbreviation"`
	} `json:"team"`
	Plays []espnPlay `json:"plays"`
}

func (e *ESPN) Plays(ctx context.Context, g Game) (Game, []Play, error) {
	e.mu.Lock()
	path := e.paths[g.ID]
	e.mu.Unlock()
	if path == "" {
		path = e.Paths[0]
	}
	var s espnSummary
	if err := getJSON(ctx, e.HTTP, e.url(path, "summary?event="+g.ID), &s); err != nil {
		return g, nil, err
	}
	cur := g
	if len(s.Header.Competitions) > 0 {
		c := s.Header.Competitions[0]
		comp := g.Competition
		cur = e.toGame(g.ID, comp, c, c.Status)
	}
	teams := map[string]string{} // team ID or display name → abbreviation
	if len(s.Header.Competitions) > 0 {
		for _, t := range s.Header.Competitions[0].Competitors {
			teams[t.Team.ID] = t.Team.Abbreviation
			teams[t.Team.DisplayName] = t.Team.Abbreviation
		}
	}

	var plays []Play
	switch e.Sport {
	case "soccer":
		plays = soccerPlays(s, teams)
	case "nfl":
		var all []espnPlay
		var driveTeam []string
		for _, d := range s.Drives.Previous {
			for _, p := range d.Plays {
				all = append(all, p)
				driveTeam = append(driveTeam, d.Team.Abbreviation)
			}
		}
		if s.Drives.Current != nil {
			for _, p := range s.Drives.Current.Plays {
				all = append(all, p)
				driveTeam = append(driveTeam, s.Drives.Current.Team.Abbreviation)
			}
		}
		seen := map[int64]bool{}
		for i, p := range all {
			pl := espnToPlay(p, teams, "Q")
			if pl.Seq == 0 || seen[pl.Seq] {
				continue // current drive can repeat the last previous play
			}
			seen[pl.Seq] = true
			if pl.Team == "" {
				pl.Team = driveTeam[i] // offense
			}
			if p.Start != nil {
				pl.Props["down_distance"] = p.Start.DownDistanceText
				pl.Props["yard_line"] = p.Start.YardLine
				if p.Start.DownDistanceText != "" {
					pl.Text = p.Start.DownDistanceText + " · " + pl.Text
				}
			}
			pl.Props["yards"] = p.StatYardage
			pl.Props["turnover"] = p.IsTurnover
			pl.Props["penalty"] = p.IsPenalty
			plays = append(plays, pl)
		}
	default: // basketball
		for _, p := range s.Plays {
			pl := espnToPlay(p, teams, "Q")
			if pl.Seq == 0 {
				continue
			}
			if p.Coordinate != nil && p.Coordinate.X > -1000 {
				pl.Props["x"], pl.Props["y"] = p.Coordinate.X, p.Coordinate.Y
			}
			plays = append(plays, pl)
		}
	}
	return cur, plays, nil
}

func espnToPlay(p espnPlay, teams map[string]string, periodPrefix string) Play {
	seq, _ := strconv.ParseInt(p.SequenceNumber, 10, 64)
	if seq == 0 {
		seq, _ = strconv.ParseInt(p.ID, 10, 64)
	}
	team := ""
	if p.Team != nil {
		team = teams[p.Team.ID]
		if team == "" {
			team = p.Team.Abbreviation
		}
	}
	actor := ""
	if len(p.Participants) > 0 {
		actor = p.Participants[0].Athlete.DisplayName
	}
	period := fmt.Sprintf("%s%d", periodPrefix, p.Period.Number)
	if p.Period.Number > 4 {
		period = "OT"
		if p.Period.Number > 5 {
			period = fmt.Sprintf("%dOT", p.Period.Number-4)
		}
	}
	return Play{
		Seq: seq, Type: typeSlug(p.Type.Text), Text: p.Text, Team: team, Actor: actor,
		Period: period, Clock: p.Clock.DisplayValue, Scoring: p.ScoringPlay,
		Props: map[string]any{
			"away_score": p.AwayScore, "home_score": p.HomeScore, "score_value": p.ScoreValue,
			"play_type": p.Type.Text,
		},
	}
}

func soccerPlays(s espnSummary, teams map[string]string) []Play {
	var plays []Play
	for _, c := range s.Commentary {
		if c.Text == "" {
			continue
		}
		pl := Play{Seq: c.Sequence + 1, Type: "commentary", Text: c.Text, Clock: c.Time.DisplayValue, Props: map[string]any{}}
		if c.Play != nil {
			pl.Type = typeSlug(c.Play.Type.Text) // goal, yellow-card, substitution, foul, shot-on-target, ...
			if c.Play.Type.Type != "" {
				pl.Type = typeSlug(c.Play.Type.Type)
			}
			pl.Scoring = c.Play.ScoringPlay
			if c.Play.Team != nil {
				pl.Team = teams[c.Play.Team.DisplayName]
				if pl.Team == "" {
					pl.Team = c.Play.Team.DisplayName
				}
			}
			if len(c.Play.Participants) > 0 {
				pl.Actor = c.Play.Participants[0].Athlete.DisplayName
			}
			switch c.Play.Period.Number {
			case 1:
				pl.Period = "1H"
			case 2:
				pl.Period = "2H"
			case 3, 4:
				pl.Period = "ET"
			case 5:
				pl.Period = "PSO"
			}
			pl.Props["play_type"] = c.Play.Type.Text
		}
		// ESPN's commentary doesn't always flag scoring plays (own goals,
		// for one), so goal-type plays count as scoring regardless.
		if strings.HasPrefix(c.Text, "Goal!") || strings.Contains(pl.Type, "goal") ||
			strings.HasPrefix(pl.Type, "penalty-scored") {
			pl.Scoring = true
		}
		plays = append(plays, pl)
	}
	return plays
}
