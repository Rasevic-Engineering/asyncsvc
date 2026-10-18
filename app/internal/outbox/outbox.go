// Package outbox implements the transactional outbox pattern on Postgres.
//
// Enqueue writes a message row inside the caller's transaction, so the message
// exists if and only if the business change commits. Relay publishes pending
// rows to Pub/Sub: callers flush their own rows right after commit (fast path),
// and a Cloud Scheduler job flushes anything left behind (safety net).
// Delivery is at-least-once; consumers must be idempotent.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/asyncsvc/internal/activity"
	"github.com/example/asyncsvc/internal/messages"
)

type Publisher interface {
	Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) (string, error)
}

func Enqueue(ctx context.Context, tx pgx.Tx, topic string, payload any, attrs map[string]string) (uuid.UUID, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, err
	}
	if attrs == nil {
		attrs = map[string]string{}
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO outbox (topic, payload, attributes) VALUES ($1, $2, $3) RETURNING id`,
		topic, data, attrs).Scan(&id)
	return id, err
}

type Relay struct {
	Pool *pgxpool.Pool
	Pub  Publisher
	// Activity, if set, records each message's "published" stage.
	Activity *activity.Recorder
}

const batchSize = 100

// Flush publishes unpublished rows. With ids, only those rows are considered;
// without, it drains the whole backlog in batches. Returns rows published.
func (r *Relay) Flush(ctx context.Context, ids ...uuid.UUID) (int, error) {
	total := 0
	for {
		n, seen, err := r.flushBatch(ctx, ids)
		total += n
		if err != nil || len(ids) > 0 || seen < batchSize || n == 0 {
			return total, err
		}
	}
}

// TryFlush is the post-commit fast path: best effort, errors are only logged
// because the scheduled sweep will retry.
func (r *Relay) TryFlush(ctx context.Context, ids ...uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := r.Flush(ctx, ids...); err != nil {
		slog.WarnContext(ctx, "outbox fast-path publish failed; sweeper will retry", "err", err)
	}
}

func (r *Relay) flushBatch(ctx context.Context, ids []uuid.UUID) (published, seen int, err error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	q := `SELECT id, topic, payload, attributes FROM outbox
	      WHERE published_at IS NULL`
	args := []any{}
	if len(ids) > 0 {
		q += ` AND id = ANY($1)`
		args = append(args, ids)
	}
	q += fmt.Sprintf(` ORDER BY created_at LIMIT %d FOR UPDATE SKIP LOCKED`, batchSize)

	type row struct {
		id      uuid.UUID
		topic   string
		payload []byte
		attrs   map[string]string
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return 0, 0, err
	}
	batch, err := pgx.CollectRows(rows, func(rr pgx.CollectableRow) (row, error) {
		var x row
		err := rr.Scan(&x.id, &x.topic, &x.payload, &x.attrs)
		return x, err
	})
	if err != nil {
		return 0, 0, err
	}

	var ok []uuid.UUID
	var firstErr error
	for _, m := range batch {
		attrs := maps.Clone(m.attrs)
		if attrs == nil {
			attrs = map[string]string{}
		}
		attrs[messages.AttrOutboxID] = m.id.String()

		msgID, err := r.Pub.Publish(ctx, m.topic, m.payload, attrs)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("publish outbox %s to %s: %w", m.id, m.topic, err)
			}
			if _, uerr := tx.Exec(ctx, `UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, m.id, err.Error()); uerr != nil {
				return 0, len(batch), uerr
			}
			continue
		}
		ok = append(ok, m.id)
		summary, reportID := activity.Describe(m.payload)
		r.Activity.Record(messages.Activity{
			Kind: messages.ActivityPublished, Topic: m.topic, MessageID: msgID,
			OutboxID: m.id.String(), Summary: summary, ReportID: reportID,
		})
	}

	if len(ok) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ok); err != nil {
			return 0, len(batch), err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, len(batch), err
	}
	return len(ok), len(batch), firstErr
}
