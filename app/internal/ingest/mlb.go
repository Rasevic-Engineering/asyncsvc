package ingest

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MLB reads the MLB Stats API (statsapi.mlb.com): the day's schedule and
// each game's live feed, pitch by pitch.
//
// Sequence: atBatIndex*1000 + the event's index within the at-bat, so every
// pitch and in-game action (pickoff, substitution, ...) is its own play, and
// the at-bat's result is published once it completes (index 999).
type MLB struct {
	BaseURL string // default https://statsapi.mlb.com
	HTTP    *http.Client
	Every   time.Duration // default 10s
	// Today returns the schedule date to read (default: today, US Eastern,
	// so late games stay "today" past midnight UTC).
	Today func() string
}

func (m *MLB) Name() string  { return "mlb" }
func (m *MLB) Label() string { return "MLB" }
func (m *MLB) PollEvery() time.Duration {
	if m.Every > 0 {
		return m.Every
	}
	return 10 * time.Second
}

func (m *MLB) url(path string) string {
	base := m.BaseURL
	if base == "" {
		base = "https://statsapi.mlb.com"
	}
	return base + path
}

func usEasternDate() string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.FixedZone("ET", -5*3600)
	}
	return time.Now().In(loc).Format("2006-01-02")
}

type mlbTeamRef struct {
	Team struct {
		Name         string `json:"name"`
		Abbreviation string `json:"abbreviation"`
	} `json:"team"`
	Score int `json:"score"`
}

func mlbState(abstract, detailed string) string {
	switch abstract {
	case "Live":
		return Live
	case "Final":
		return Final
	}
	if detailed == "In Progress" {
		return Live
	}
	return Scheduled
}

func (m *MLB) Schedule(ctx context.Context) ([]Game, error) {
	date := usEasternDate()
	if m.Today != nil {
		date = m.Today()
	}
	var sched struct {
		Dates []struct {
			Games []struct {
				GamePk   int64  `json:"gamePk"`
				GameDate string `json:"gameDate"`
				Status   struct {
					AbstractGameState string `json:"abstractGameState"`
					DetailedState     string `json:"detailedState"`
				} `json:"status"`
				Teams struct {
					Away mlbTeamRef `json:"away"`
					Home mlbTeamRef `json:"home"`
				} `json:"teams"`
				SeriesDescription string `json:"seriesDescription"`
			} `json:"games"`
		} `json:"dates"`
	}
	if err := getJSON(ctx, m.HTTP, m.url("/api/v1/schedule?sportId=1&hydrate=team&date="+date), &sched); err != nil {
		return nil, err
	}
	var out []Game
	for _, d := range sched.Dates {
		for _, g := range d.Games {
			game := Game{
				League: "mlb", ID: strconv.FormatInt(g.GamePk, 10),
				State:       mlbState(g.Status.AbstractGameState, g.Status.DetailedState),
				StatusText:  g.Status.DetailedState,
				StartTime:   g.GameDate,
				Competition: g.SeriesDescription,
				Home:        Team{Abbr: abbrOr(g.Teams.Home.Team.Abbreviation, g.Teams.Home.Team.Name), Name: g.Teams.Home.Team.Name, Score: g.Teams.Home.Score},
				Away:        Team{Abbr: abbrOr(g.Teams.Away.Team.Abbreviation, g.Teams.Away.Team.Name), Name: g.Teams.Away.Team.Name, Score: g.Teams.Away.Score},
			}
			out = append(out, game)
		}
	}
	return out, nil
}

func abbrOr(abbr, name string) string {
	if abbr != "" {
		return abbr
	}
	return name
}

type mlbFeed struct {
	GameData struct {
		Status struct {
			AbstractGameState string `json:"abstractGameState"`
			DetailedState     string `json:"detailedState"`
		} `json:"status"`
		Datetime struct {
			DateTime string `json:"dateTime"`
		} `json:"datetime"`
		Teams struct {
			Away struct {
				Name         string `json:"name"`
				Abbreviation string `json:"abbreviation"`
			} `json:"away"`
			Home struct {
				Name         string `json:"name"`
				Abbreviation string `json:"abbreviation"`
			} `json:"home"`
		} `json:"teams"`
	} `json:"gameData"`
	LiveData struct {
		Linescore struct {
			CurrentInning        int    `json:"currentInning"`
			CurrentInningOrdinal string `json:"currentInningOrdinal"`
			InningHalf           string `json:"inningHalf"`
			Balls                int    `json:"balls"`
			Strikes              int    `json:"strikes"`
			Outs                 int    `json:"outs"`
			Teams                struct {
				Away struct {
					Runs int `json:"runs"`
					Hits int `json:"hits"`
				} `json:"away"`
				Home struct {
					Runs int `json:"runs"`
					Hits int `json:"hits"`
				} `json:"home"`
			} `json:"teams"`
		} `json:"linescore"`
		Plays struct {
			AllPlays []mlbPlay `json:"allPlays"`
		} `json:"plays"`
	} `json:"liveData"`
}

type mlbPlay struct {
	Result struct {
		Event       string `json:"event"`
		EventType   string `json:"eventType"`
		Description string `json:"description"`
		RBI         int    `json:"rbi"`
		AwayScore   int    `json:"awayScore"`
		HomeScore   int    `json:"homeScore"`
	} `json:"result"`
	About struct {
		AtBatIndex int64  `json:"atBatIndex"`
		HalfInning string `json:"halfInning"` // "top" / "bottom"
		Inning     int    `json:"inning"`
		IsComplete bool   `json:"isComplete"`
		IsScoring  bool   `json:"isScoringPlay"`
	} `json:"about"`
	Matchup struct {
		Batter struct {
			FullName string `json:"fullName"`
		} `json:"batter"`
		Pitcher struct {
			FullName string `json:"fullName"`
		} `json:"pitcher"`
	} `json:"matchup"`
	PlayEvents []struct {
		Index   int64 `json:"index"`
		IsPitch bool  `json:"isPitch"`
		Details struct {
			Description string `json:"description"`
			Event       string `json:"event"`
			Type        *struct {
				Description string `json:"description"`
			} `json:"type"`
			IsStrike bool `json:"isStrike"`
			IsBall   bool `json:"isBall"`
			IsInPlay bool `json:"isInPlay"`
		} `json:"details"`
		Count struct {
			Balls   int `json:"balls"`
			Strikes int `json:"strikes"`
			Outs    int `json:"outs"`
		} `json:"count"`
		PitchData *struct {
			StartSpeed float64 `json:"startSpeed"`
			Zone       int     `json:"zone"`
			Coords     struct {
				PX float64 `json:"pX"`
				PZ float64 `json:"pZ"`
			} `json:"coordinates"`
		} `json:"pitchData"`
		HitData *struct {
			LaunchSpeed float64 `json:"launchSpeed"`
			LaunchAngle float64 `json:"launchAngle"`
			TotalDist   float64 `json:"totalDistance"`
		} `json:"hitData"`
	} `json:"playEvents"`
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

func mlbHalf(half string, inning int) string {
	if strings.HasPrefix(strings.ToLower(half), "top") {
		return "Top " + ordinal(inning)
	}
	return "Bot " + ordinal(inning)
}

func (m *MLB) Plays(ctx context.Context, g Game) (Game, []Play, error) {
	var f mlbFeed
	if err := getJSON(ctx, m.HTTP, m.url("/api/v1.1/game/"+g.ID+"/feed/live"), &f); err != nil {
		return g, nil, err
	}
	gd, ls := f.GameData, f.LiveData.Linescore
	cur := g
	cur.State = mlbState(gd.Status.AbstractGameState, gd.Status.DetailedState)
	cur.Away = Team{Abbr: abbrOr(gd.Teams.Away.Abbreviation, g.Away.Abbr), Name: gd.Teams.Away.Name, Score: ls.Teams.Away.Runs}
	cur.Home = Team{Abbr: abbrOr(gd.Teams.Home.Abbreviation, g.Home.Abbr), Name: gd.Teams.Home.Name, Score: ls.Teams.Home.Runs}
	switch cur.State {
	case Final:
		cur.StatusText = "Final"
	case Live:
		cur.StatusText = mlbHalf(ls.InningHalf, ls.CurrentInning) + fmt.Sprintf(" · %d-%d, %d out", ls.Balls, ls.Strikes, ls.Outs)
	default:
		cur.StatusText = gd.Status.DetailedState
	}
	cur.Extra = map[string]any{"away_hits": ls.Teams.Away.Hits, "home_hits": ls.Teams.Home.Hits}

	var plays []Play
	for _, p := range f.LiveData.Plays.AllPlays {
		// Top of the inning: the away team bats.
		batting, fielding := cur.Home.Abbr, cur.Away.Abbr
		if strings.ToLower(p.About.HalfInning) == "top" {
			batting, fielding = cur.Away.Abbr, cur.Home.Abbr
		}
		period := mlbHalf(p.About.HalfInning, p.About.Inning)
		base := p.About.AtBatIndex * 1000
		batter, pitcher := p.Matchup.Batter.FullName, p.Matchup.Pitcher.FullName

		for _, e := range p.PlayEvents {
			if e.Index >= 999 {
				continue
			}
			count := fmt.Sprintf("%d-%d", e.Count.Balls, e.Count.Strikes)
			props := map[string]any{
				"batter": batter, "pitcher": pitcher, "inning": p.About.Inning, "half": p.About.HalfInning,
				"balls": e.Count.Balls, "strikes": e.Count.Strikes, "outs": e.Count.Outs,
				"call": e.Details.Description,
			}
			if e.IsPitch {
				pitchType := ""
				if e.Details.Type != nil {
					pitchType = e.Details.Type.Description
				}
				text := e.Details.Description
				if e.PitchData != nil && e.PitchData.StartSpeed > 0 {
					text = fmt.Sprintf("%.1f mph %s · %s", e.PitchData.StartSpeed, pitchType, e.Details.Description)
					props["mph"] = e.PitchData.StartSpeed
					props["zone"] = e.PitchData.Zone
					props["px"], props["pz"] = e.PitchData.Coords.PX, e.PitchData.Coords.PZ
				}
				props["pitch_type"] = pitchType
				if e.HitData != nil && e.HitData.LaunchSpeed > 0 {
					props["exit_velocity"] = e.HitData.LaunchSpeed
					props["launch_angle"] = e.HitData.LaunchAngle
					props["distance"] = e.HitData.TotalDist
				}
				plays = append(plays, Play{
					Seq: base + e.Index, Type: "pitch", Team: fielding, Actor: pitcher,
					Text:   fmt.Sprintf("%s to %s: %s (%s)", pitcher, batter, text, count),
					Period: period, Clock: count, Props: props,
				})
				continue
			}
			// In-game action: pickoff, stolen base, substitution, mound visit, ...
			desc := e.Details.Description
			if desc == "" {
				desc = e.Details.Event
			}
			if desc == "" {
				continue
			}
			plays = append(plays, Play{
				Seq: base + e.Index, Type: "action", Team: batting, Actor: batter,
				Text: desc, Period: period, Clock: count, Props: props,
			})
		}

		if p.About.IsComplete && p.Result.Event != "" {
			plays = append(plays, Play{
				Seq:     base + 999,
				Type:    typeSlug(p.Result.Event), // single, strikeout, home-run, ...
				Text:    p.Result.Description,
				Team:    batting,
				Actor:   batter,
				Period:  period,
				Clock:   fmt.Sprintf("%d–%d", p.Result.AwayScore, p.Result.HomeScore),
				Scoring: p.About.IsScoring,
				Props: map[string]any{
					"batter": batter, "pitcher": pitcher, "inning": p.About.Inning, "half": p.About.HalfInning,
					"event": p.Result.Event, "rbi": p.Result.RBI,
					"away_score": p.Result.AwayScore, "home_score": p.Result.HomeScore,
				},
			})
		}
	}
	return cur, plays, nil
}
