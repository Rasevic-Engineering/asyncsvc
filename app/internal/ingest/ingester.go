package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/example/asyncsvc/internal/activity"
	"github.com/example/asyncsvc/internal/messages"
	"github.com/example/asyncsvc/internal/pubsubx"
)

// MaxReplays is how many replays may run at once.
const MaxReplays = 3

// Ingester runs the live league tracker and handles replay commands.
type Ingester struct {
	Tracker   *Tracker
	StatsBomb *StatsBomb
	Status    StatusStore

	mu      sync.Mutex
	replays map[string]context.CancelCauseFunc
	wg      sync.WaitGroup
	ctx     context.Context
}

// New wires the sources to the events topic and the status store. Set
// Tracker.Leagues to choose live leagues.
func New(pub Publisher, eventsTopic string, rec *activity.Recorder, status StatusStore) *Ingester {
	sk := &sink{pub: pub, topic: eventsTopic, activity: rec}
	return &Ingester{
		Tracker:   &Tracker{sink: sk, status: status},
		StatsBomb: &StatsBomb{sink: sk, status: status},
		Status:    status,
		replays:   map[string]context.CancelCauseFunc{},
	}
}

// Start resumes replays interrupted by a restart, and begins polling the
// live leagues (if any). It returns immediately; Wait blocks until ctx ends.
func (in *Ingester) Start(ctx context.Context) {
	in.ctx = ctx
	running, err := in.Status.RunningReplays(ctx)
	if err != nil {
		slog.WarnContext(ctx, "list running replays", "err", err)
	}
	for _, doc := range running {
		cmd := messages.IngestCommand{
			Action:      "replay",
			ReplayID:    fmt.Sprint(doc["replay_id"]),
			MatchID:     int(toInt64(doc["match_id"], 0)),
			Speed:       int(toInt64(doc["speed"], 10)),
			RequestedBy: fmt.Sprint(doc["requested_by"]),
		}
		slog.InfoContext(ctx, "resuming replay", "replay_id", cmd.ReplayID)
		in.startReplay(cmd, int(toInt64(doc["emitted"], 0)))
	}
	if len(in.Tracker.Leagues) > 0 {
		in.wg.Add(1)
		go func() {
			defer in.wg.Done()
			in.Tracker.Run(ctx)
		}()
	}
}

func (in *Ingester) Wait() { in.wg.Wait() }

// HandleCommand processes one ingest-commands message. Unknown or invalid
// commands are dropped (acked); there's nothing to retry.
func (in *Ingester) HandleCommand(ctx context.Context, d pubsubx.Delivery) error {
	var cmd messages.IngestCommand
	if err := json.Unmarshal(d.Data, &cmd); err != nil || cmd.ReplayID == "" {
		slog.WarnContext(ctx, "bad ingest command; dropping", "err", err)
		return nil
	}
	doc := replayDocID(cmd.ReplayID)

	switch cmd.Action {
	case "replay":
		if _, ok := CatalogMatch(cmd.MatchID); !ok || !ValidSpeed(cmd.Speed) {
			return in.Status.Merge(ctx, doc, map[string]any{"kind": "statsbomb", "replay_id": cmd.ReplayID,
				"state": "failed", "error": "unknown match or speed"})
		}
		// Redelivered command for a replay we already know: don't start twice.
		if existing, ok, err := in.Status.Get(ctx, doc); err != nil {
			return err
		} else if ok && existing["state"] != nil {
			return nil
		}
		in.mu.Lock()
		busy := len(in.replays) >= MaxReplays
		in.mu.Unlock()
		if busy {
			return in.Status.Merge(ctx, doc, map[string]any{"kind": "statsbomb", "replay_id": cmd.ReplayID,
				"match_id": cmd.MatchID, "state": "failed",
				"error": fmt.Sprintf("%d replays already running; stop one first", MaxReplays)})
		}
		in.startReplay(cmd, 0)
		return nil

	case "stop":
		in.mu.Lock()
		cancel, ok := in.replays[cmd.ReplayID]
		in.mu.Unlock()
		if ok {
			cancel(ErrStopped) // the replay records "stopped" itself
			return nil
		}
		// Not running here (already finished, or lost): just mark it.
		existing, found, err := in.Status.Get(ctx, doc)
		if err != nil {
			return err
		}
		if found && existing["state"] == "running" {
			return in.Status.Merge(ctx, doc, map[string]any{"state": "stopped"})
		}
		return nil
	}
	slog.WarnContext(ctx, "unknown ingest command; dropping", "action", cmd.Action)
	return nil
}

func (in *Ingester) startReplay(cmd messages.IngestCommand, from int) {
	rctx, cancel := context.WithCancelCause(in.ctx)
	in.mu.Lock()
	in.replays[cmd.ReplayID] = cancel
	in.mu.Unlock()

	in.wg.Add(1)
	go func() {
		defer in.wg.Done()
		defer func() {
			cancel(nil)
			in.mu.Lock()
			delete(in.replays, cmd.ReplayID)
			in.mu.Unlock()
		}()
		if err := in.StatsBomb.Replay(rctx, cmd, from); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("replay failed", "replay_id", cmd.ReplayID, "err", err)
		}
	}()
}
