package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/example/asyncsvc/internal/activity"
	"github.com/example/asyncsvc/internal/messages"
)

// Publisher publishes and waits for the server's ack.
type Publisher interface {
	Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) (string, error)
}

// eventIDs are deterministic, so re-publishing the same play (e.g. after a
// restart) produces the same event_id and duplicates are easy to remove.
var eventNamespace = uuid.MustParse("6f1d4a2e-8c3b-4f6a-9d2e-1b7c5a0e9f31")

func eventID(parts ...string) string {
	return uuid.NewSHA1(eventNamespace, []byte(strings.Join(parts, ":"))).String()
}

// sink publishes batches of events to the events topic and records one
// activity event per batch.
type sink struct {
	pub      Publisher
	topic    string
	activity *activity.Recorder
}

func (s *sink) publish(ctx context.Context, label string, events []messages.Event) error {
	if len(events) == 0 {
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(16)
	types := map[string]int{}
	for _, ev := range events {
		types[ev.EventType]++
		data, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		attrs := map[string]string{"event_type": ev.EventType, "source": ev.Source}
		g.Go(func() error {
			_, err := s.pub.Publish(gctx, s.topic, data, attrs)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("publish %s: %w", label, err)
	}
	s.activity.Record(messages.Activity{
		Kind:    messages.ActivityPublished,
		Topic:   s.topic,
		Summary: fmt.Sprintf("%s: %d events → BigQuery (%s)", label, len(events), topTypes(types, 3)),
	})
	return nil
}

func topTypes(types map[string]int, n int) string {
	keys := make([]string, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if types[keys[i]] != types[keys[j]] {
			return types[keys[i]] > types[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var parts []string
	for i, k := range keys {
		if i == n {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", strings.TrimPrefix(strings.TrimPrefix(k, "nhl."), "soccer."), types[k]))
	}
	return strings.Join(parts, ", ")
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
