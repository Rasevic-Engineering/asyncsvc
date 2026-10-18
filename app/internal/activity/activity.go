// Package activity records the lifecycle of Pub/Sub messages (published,
// received, acked, nacked, ...) for the admin Activity page.
//
// Producers call Recorder.Record, which publishes an Activity to the
// activity topic without waiting, so recording never slows down or fails
// real processing. The topic fans out to:
//   - the worker, which writes each event to Firestore (FirestoreSink);
//     the admin UI listens to that collection in real time, and
//   - a BigQuery subscription that keeps the full history.
package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"

	"github.com/example/asyncsvc/internal/messages"
)

// Publisher publishes without waiting for the result.
type Publisher interface {
	PublishAsync(topic string, data []byte, attrs map[string]string)
}

// Recorder publishes activity events. A nil Recorder, or one without a
// publisher or topic, records nothing.
type Recorder struct {
	Pub     Publisher
	Topic   string
	Service string // "api" or "worker"
}

func (r *Recorder) Record(a messages.Activity) {
	if r == nil || r.Pub == nil || r.Topic == "" {
		return
	}
	if a.At == 0 {
		a.At = time.Now().UnixMicro()
	}
	a.Service = r.Service
	data, err := json.Marshal(a)
	if err != nil {
		return
	}
	r.Pub.PublishAsync(r.Topic, data, map[string]string{"kind": a.Kind, "topic": a.Topic})
}

// Describe summarizes a message payload for display, and extracts the
// report it concerns, if any.
func Describe(data []byte) (summary, reportID string) {
	var p struct {
		ReportID string `json:"report_id"`
		Template string `json:"template"`
		To       string `json:"to"`
		Data     struct {
			Kind string `json:"kind"`
		} `json:"data"`
		EventType string `json:"event_type"`
	}
	if json.Unmarshal(data, &p) != nil {
		return "", ""
	}
	switch {
	case p.ReportID != "":
		return "report request", p.ReportID
	case p.Template != "":
		s := fmt.Sprintf("email %s to %s", p.Template, p.To)
		if p.Data.Kind != "" {
			s += " (" + p.Data.Kind + ")"
		}
		return s, ""
	case p.EventType != "":
		return "event " + p.EventType, ""
	}
	return "", ""
}

// Sink stores activity events delivered by the activity subscription.
type Sink interface {
	Write(ctx context.Context, a messages.Activity) error
}

// Retention is how long events stay in the live feed; a Firestore TTL policy
// on expire_at deletes them afterwards. BigQuery keeps the full history.
const Retention = 7 * 24 * time.Hour

// Collection is the Firestore collection the admin Activity page listens to.
const Collection = "activity"

// FirestoreSink writes events to Firestore, where the admin UI receives them
// in real time through the Firebase SDK. Firestore security rules allow
// only admins to read, and nobody to write from a browser.
type FirestoreSink struct {
	Client *firestore.Client
}

func (f *FirestoreSink) Write(ctx context.Context, a messages.Activity) error {
	at := time.UnixMicro(a.At).UTC()
	doc := map[string]any{
		"at":        at,
		"expire_at": at.Add(Retention),
		"kind":      a.Kind,
		"topic":     a.Topic,
		"service":   a.Service,
	}
	for k, v := range map[string]string{
		"subscription": a.Subscription, "message_id": a.MessageID, "outbox_id": a.OutboxID,
		"summary": a.Summary, "report_id": a.ReportID, "error": a.Error,
	} {
		if v != "" {
			doc[k] = v
		}
	}
	if a.Attempt > 0 {
		doc["attempt"] = a.Attempt
	}
	if a.DurationMs > 0 {
		doc["duration_ms"] = a.DurationMs
	}
	_, _, err := f.Client.Collection(Collection).Add(ctx, doc)
	return err
}
