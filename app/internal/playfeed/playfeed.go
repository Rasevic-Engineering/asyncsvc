// Package playfeed keeps a short-lived copy of live sports plays in
// Firestore for the admin Live data page's per-sport event streams.
//
// It's a Pub/Sub consumer: the events-to-firestore subscription delivers
// events from the events topic that the ingester tagged for the UI
// (attribute ui=true), and the worker writes each one here. The same events
// also flow to BigQuery through their own subscription, so this is plain
// fan-out: one publish, two consumers.
package playfeed

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"

	"github.com/example/asyncsvc/internal/messages"
)

// Collection and retention (Firestore TTL on expire_at).
const (
	Collection = "plays"
	Retention  = 2 * 24 * time.Hour
)

// Doc converts an event into the document the UI reads. The event ID is the
// document ID, so a redelivered message just overwrites the same document.
func Doc(ev messages.Event) (id string, doc map[string]any) {
	var props map[string]any
	_ = json.Unmarshal([]byte(ev.Properties), &props)
	str := func(k string) string { s, _ := props[k].(string); return s }

	at := time.UnixMicro(ev.OccurredAt).UTC()
	doc = map[string]any{
		"league":    ev.Source, // nhl, mlb, nba, nfl, soccer, statsbomb
		"game_id":   ev.GameID,
		"type":      strings.TrimPrefix(ev.EventType, ev.Source+"."),
		"text":      str("text"),
		"at":        at,
		"expire_at": at.Add(Retention),
		"scoring":   props["scoring"] == true,
	}
	if ev.Source == "statsbomb" {
		doc["type"] = strings.TrimPrefix(ev.EventType, "soccer.")
	}
	for k, v := range map[string]string{
		"team": ev.Team, "actor": ev.UserID,
		"period": str("period"), "clock": str("clock"), "competition": str("competition"),
		"matchup": firstNonEmpty(str("game"), str("match")), "replay_id": str("replay_id"),
	} {
		if v != "" && !strings.HasPrefix(v, "team:") && v != "game" {
			doc[k] = v
		}
	}
	if s, ok := props["seq"].(float64); ok {
		doc["seq"] = int64(s)
	}
	return ev.EventID, doc
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Sink stores play documents.
type Sink interface {
	Write(ctx context.Context, ev messages.Event) error
}

// Firestore writes play documents to Firestore.
type Firestore struct {
	Client *firestore.Client
}

func (f *Firestore) Write(ctx context.Context, ev messages.Event) error {
	id, doc := Doc(ev)
	_, err := f.Client.Collection(Collection).Doc(id).Set(ctx, doc)
	return err
}

// Memory stores play documents in memory (tests).
type Memory struct {
	mu   sync.Mutex
	Docs map[string]map[string]any
}

func (m *Memory) Write(_ context.Context, ev messages.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Docs == nil {
		m.Docs = map[string]map[string]any{}
	}
	id, doc := Doc(ev)
	m.Docs[id] = doc
	return nil
}

func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Docs)
}
