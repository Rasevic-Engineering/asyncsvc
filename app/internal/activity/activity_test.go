package activity

import (
	"encoding/json"
	"testing"

	"github.com/example/asyncsvc/internal/messages"
)

type capture struct {
	topic string
	data  []byte
}

func (c *capture) PublishAsync(topic string, data []byte, _ map[string]string) {
	c.topic, c.data = topic, data
}

func TestRecorder(t *testing.T) {
	c := &capture{}
	r := &Recorder{Pub: c, Topic: "activity", Service: "worker"}
	r.Record(messages.Activity{Kind: messages.ActivityAcked, Topic: "report-requests", DurationMs: 12})

	var a messages.Activity
	if err := json.Unmarshal(c.data, &a); err != nil || c.topic != "activity" {
		t.Fatalf("%v %s", err, c.topic)
	}
	if a.Service != "worker" || a.At == 0 || a.Kind != "acked" || a.DurationMs != 12 {
		t.Fatalf("%+v", a)
	}

	var nilRec *Recorder
	nilRec.Record(messages.Activity{}) // must not panic
	(&Recorder{Pub: c}).Record(messages.Activity{})
}

func TestDescribe(t *testing.T) {
	cases := []struct{ in, summary, report string }{
		{`{"report_id":"7b1f0e5c-0000-4000-8000-000000000001"}`, "report request", "7b1f0e5c-0000-4000-8000-000000000001"},
		{`{"template":"report_ready","to":"a@example.com","data":{"kind":"raw_events"}}`, "email report_ready to a@example.com (raw_events)", ""},
		{`{"event_type":"signup","user_id":"u1"}`, "event signup", ""},
		{`not json`, "", ""},
	}
	for _, c := range cases {
		s, r := Describe([]byte(c.in))
		if s != c.summary || r != c.report {
			t.Errorf("Describe(%s) = %q, %q", c.in, s, r)
		}
	}
}
