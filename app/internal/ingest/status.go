// Package ingest feeds live sports data into the events pipeline:
//
//   - NHL: polls today's schedule and each live game's play-by-play, and
//     publishes new plays as events.
//   - StatsBomb: replays a historical soccer match's events at a chosen
//     speed, on an admin's request (ingest-commands topic).
//
// Events go to the events topic (→ BigQuery) like any other event. Progress
// is written to Firestore ("ingest" collection), which the admin Live data
// page listens to, and which lets replays resume after a restart.
package ingest

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/example/asyncsvc/internal/playfeed"
)

// StatusCollection is the Firestore collection with one document per
// tracked NHL game ("nhl-<gameId>") and per replay ("replay-<id>").
const StatusCollection = "ingest"

// StatusRetention: status documents expire (Firestore TTL on expire_at).
const StatusRetention = 7 * 24 * time.Hour

// StatusStore persists ingestion progress.
type StatusStore interface {
	// Merge sets the given fields on a document, creating it if needed.
	Merge(ctx context.Context, id string, fields map[string]any) error
	Get(ctx context.Context, id string) (map[string]any, bool, error)
	// RunningReplays returns the status of replays left in state "running".
	RunningReplays(ctx context.Context) ([]map[string]any, error)
	// DeleteReplay removes a replay's status document and its highlight
	// plays (the UI's copy). Its events stay in BigQuery.
	DeleteReplay(ctx context.Context, replayID string) error
}

func withTimes(fields map[string]any) map[string]any {
	out := maps.Clone(fields)
	now := time.Now().UTC()
	out["updated_at"] = now
	out["expire_at"] = now.Add(StatusRetention)
	return out
}

// FirestoreStatus stores status in Firestore.
type FirestoreStatus struct {
	Client *firestore.Client
}

func (f *FirestoreStatus) Merge(ctx context.Context, id string, fields map[string]any) error {
	_, err := f.Client.Collection(StatusCollection).Doc(id).Set(ctx, withTimes(fields), firestore.MergeAll)
	return err
}

func (f *FirestoreStatus) Get(ctx context.Context, id string) (map[string]any, bool, error) {
	snap, err := f.Client.Collection(StatusCollection).Doc(id).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return snap.Data(), true, nil
}

func (f *FirestoreStatus) RunningReplays(ctx context.Context) ([]map[string]any, error) {
	it := f.Client.Collection(StatusCollection).
		Where("kind", "==", "statsbomb").Where("state", "==", "running").Documents(ctx)
	defer it.Stop()
	var out []map[string]any
	for {
		snap, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, snap.Data())
	}
}

func (f *FirestoreStatus) DeleteReplay(ctx context.Context, replayID string) error {
	bw := f.Client.BulkWriter(ctx)
	it := f.Client.Collection(playfeed.Collection).Where("replay_id", "==", replayID).Documents(ctx)
	defer it.Stop()
	for {
		snap, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			bw.End()
			return err
		}
		if _, err := bw.Delete(snap.Ref); err != nil {
			bw.End()
			return err
		}
	}
	bw.End()
	// Deleting a missing document is not an error.
	_, err := f.Client.Collection(StatusCollection).Doc(replayDocID(replayID)).Delete(ctx)
	return err
}

// MemoryStatus is an in-memory StatusStore for tests.
type MemoryStatus struct {
	mu   sync.Mutex
	Docs map[string]map[string]any
}

func (m *MemoryStatus) Merge(_ context.Context, id string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Docs == nil {
		m.Docs = map[string]map[string]any{}
	}
	d := m.Docs[id]
	if d == nil {
		d = map[string]any{}
		m.Docs[id] = d
	}
	maps.Copy(d, withTimes(fields))
	return nil
}

func (m *MemoryStatus) Get(_ context.Context, id string) (map[string]any, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.Docs[id]
	return maps.Clone(d), ok, nil
}

func (m *MemoryStatus) RunningReplays(context.Context) ([]map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]any
	for _, d := range m.Docs {
		if d["kind"] == "statsbomb" && d["state"] == "running" {
			out = append(out, maps.Clone(d))
		}
	}
	return out, nil
}

func (m *MemoryStatus) DeleteReplay(_ context.Context, replayID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Docs, replayDocID(replayID))
	return nil
}

// Snapshot returns a copy of a document (tests).
func (m *MemoryStatus) Snapshot(id string) map[string]any {
	d, _, _ := m.Get(context.Background(), id)
	return d
}
