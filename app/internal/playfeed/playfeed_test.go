package playfeed

import (
	"testing"
	"time"

	"github.com/example/asyncsvc/internal/messages"
)

func TestDoc(t *testing.T) {
	at := time.Date(2026, 10, 19, 1, 2, 3, 0, time.UTC)
	id, d := Doc(messages.Event{
		EventID: "e1", EventType: "nhl.goal", UserID: "Kirill Kaprizov", Source: "nhl", GameID: "nhl:1", Team: "MIN",
		OccurredAt: at.UnixMicro(),
		Properties: `{"game":"CBJ @ MIN","period":"P2","clock":"01:10","text":"Goal · Kirill Kaprizov (MIN)","scoring":true,"seq":80}`,
	})
	if id != "e1" || d["league"] != "nhl" || d["type"] != "goal" || d["matchup"] != "CBJ @ MIN" || d["scoring"] != true ||
		d["actor"] != "Kirill Kaprizov" || d["seq"] != int64(80) || d["at"] != at || d["expire_at"] != at.Add(Retention) {
		t.Fatalf("%v", d)
	}

	_, r := Doc(messages.Event{
		EventID: "e2", EventType: "soccer.shot", UserID: "team:Argentina", Source: "statsbomb", Team: "Argentina",
		Properties: `{"match":"Argentina 3–3 France","replay_id":"r1","text":"Goal! Shot by Messi","clock":"23'","highlight":true,"scoring":true}`,
	})
	if r["type"] != "shot" || r["matchup"] != "Argentina 3–3 France" || r["replay_id"] != "r1" || r["actor"] != nil {
		t.Fatalf("replay doc: %v", r)
	}
}
