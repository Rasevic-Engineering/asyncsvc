// Package worker processes Pub/Sub messages and sweeps the outbox.
//
// Messages arrive either by push (Cloud Run: a 2xx response acks, anything
// else nacks) or by streaming pull (GKE: the handler's error decides ack or
// nack). Either way, Pub/Sub redelivers nacked messages with exponential
// backoff until the subscription's max delivery attempts, then sends them to
// the dead-letter topic. Every handler is idempotent because delivery is
// at-least-once.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/asyncsvc/internal/blob"
	"github.com/example/asyncsvc/internal/httpx"
	"github.com/example/asyncsvc/internal/mail"
	"github.com/example/asyncsvc/internal/messages"
	"github.com/example/asyncsvc/internal/outbox"
	"github.com/example/asyncsvc/internal/pubsubx"
	"github.com/example/asyncsvc/internal/reports"
)

type Worker struct {
	Pool         *pgxpool.Pool
	Reports      *reports.Repo
	Source       reports.Source
	Store        blob.Store
	Mailer       mail.Mailer
	Relay        *outbox.Relay
	TopicEmails  string
	PublicAPIURL string

	// MaxAttempts must match the reports subscription's max delivery attempts.
	MaxAttempts int
	// GenerateTimeout bounds one report run.
	GenerateTimeout time.Duration
	// StaleAfter: a report "running" longer than this is assumed abandoned
	// (e.g. the instance crashed) and may be reclaimed. Must exceed
	// GenerateTimeout.
	StaleAfter time.Duration
}

func (wk *Worker) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", httpx.Health)
	mux.HandleFunc("GET /healthz", httpx.Health)
	mux.HandleFunc("POST /pubsub/reports", wk.push(wk.Handle(wk.HandleReport)))
	mux.HandleFunc("POST /pubsub/emails", wk.push(wk.Handle(wk.HandleEmail)))
	mux.HandleFunc("POST /tasks/relay-outbox", wk.relayOutbox)
	return mux
}

// errRetryLater nacks without logging an error.
var errRetryLater = errors.New("retry later")

// Handle wraps a message handler with logging. Used for both push and pull.
func (wk *Worker) Handle(h pubsubx.Handler) pubsubx.Handler {
	return func(ctx context.Context, d pubsubx.Delivery) error {
		err := h(ctx, d)
		if err == nil {
			return nil
		}
		log := slog.With("message_id", d.MessageID, "attempt", d.Attempt, "subscription", d.Subscription)
		if errors.Is(err, errRetryLater) {
			log.InfoContext(ctx, "nack: retry later")
		} else {
			log.ErrorContext(ctx, "nack: handler failed", "err", err)
		}
		return err
	}
}

// push adapts a handler to a Pub/Sub push HTTP endpoint.
func (wk *Worker) push(h pubsubx.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := pubsubx.DecodePush(r)
		if err != nil {
			// Malformed envelope will never succeed: ack it so it doesn't loop.
			slog.ErrorContext(r.Context(), "bad push envelope; acking", "err", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := h(r.Context(), d); err != nil {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (wk *Worker) HandleReport(ctx context.Context, d pubsubx.Delivery) error {
	var msg messages.ReportRequested
	if err := json.Unmarshal(d.Data, &msg); err != nil {
		slog.ErrorContext(ctx, "bad report message; acking", "err", err)
		return nil
	}
	log := slog.With("report_id", msg.ReportID, "attempt", d.Attempt)

	rep, claimed, err := wk.Reports.Claim(ctx, msg.ReportID, wk.StaleAfter)
	if errors.Is(err, reports.ErrNotFound) {
		log.WarnContext(ctx, "report not found; acking")
		return nil
	}
	if err != nil {
		return err
	}
	if !claimed {
		switch rep.Status {
		case reports.StatusReady, reports.StatusFailed:
			log.InfoContext(ctx, "duplicate delivery for finished report; acking")
			return nil
		default:
			// Another delivery is generating it right now (or crashed recently
			// and the claim isn't stale yet). Let Pub/Sub back off and retry.
			return errRetryLater
		}
	}

	log.InfoContext(ctx, "generating report", "kind", rep.Kind)
	key := fmt.Sprintf("reports/%s/%s.csv", rep.ID, rep.Kind)
	genCtx, cancel := context.WithTimeout(ctx, wk.GenerateTimeout)
	defer cancel()

	var rows int64
	err = wk.Store.Put(genCtx, key, "text/csv", func(w io.Writer) error {
		n, err := wk.Source.Generate(genCtx, rep.Kind, rep.Params, w)
		rows = n
		return err
	})

	switch {
	case err == nil:
		return wk.finish(ctx, rep, key, rows)

	case errors.Is(err, reports.ErrInvalid):
		log.WarnContext(ctx, "report request invalid; failing permanently", "err", err)
		return wk.fail(ctx, rep, err.Error(), nil)

	case d.Attempt >= wk.MaxAttempts:
		log.ErrorContext(ctx, "report failed on final attempt", "err", err)
		// Record the failure and still nack, so the message lands in the
		// dead-letter topic for inspection.
		return wk.fail(ctx, rep, "generation failed after retries", err)

	default:
		// Use a fresh context: ctx may be the reason we failed.
		if rerr := wk.Reports.Release(context.WithoutCancel(ctx), rep.ID, err.Error()); rerr != nil {
			log.ErrorContext(ctx, "release report", "err", rerr)
		}
		return fmt.Errorf("generate report: %w", err)
	}
}

// finish marks the report ready and queues the notification email atomically.
func (wk *Worker) finish(ctx context.Context, rep reports.Report, key string, rows int64) error {
	tx, err := wk.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := wk.Reports.MarkReady(ctx, tx, rep.ID, key, rows); err != nil {
		return err
	}
	var ids []uuid.UUID
	if rep.NotifyEmail != nil {
		id, err := outbox.Enqueue(ctx, tx, wk.TopicEmails, messages.EmailRequested{
			Template: "report_ready",
			To:       *rep.NotifyEmail,
			Data: map[string]string{
				"kind": rep.Kind, "from": rep.Params.From, "to": rep.Params.To,
				"rows": strconv.FormatInt(rows, 10),
				"url":  wk.PublicAPIURL + "/v1/reports/" + rep.ID.String(),
			},
		}, nil)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	slog.InfoContext(ctx, "report ready", "report_id", rep.ID, "rows", rows, "key", key)
	if len(ids) > 0 {
		wk.Relay.TryFlush(ctx, ids...)
	}
	return nil
}

// fail marks the report failed and queues a failure email. It returns retErr
// so the caller can choose between ack (nil) and nack (dead-letter).
func (wk *Worker) fail(ctx context.Context, rep reports.Report, reason string, retErr error) error {
	ctx = context.WithoutCancel(ctx)
	tx, err := wk.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := wk.Reports.MarkFailed(ctx, tx, rep.ID, reason); err != nil {
		return err
	}
	var ids []uuid.UUID
	if rep.NotifyEmail != nil {
		id, err := outbox.Enqueue(ctx, tx, wk.TopicEmails, messages.EmailRequested{
			Template: "report_failed",
			To:       *rep.NotifyEmail,
			Data:     map[string]string{"kind": rep.Kind, "from": rep.Params.From, "to": rep.Params.To, "error": reason},
		}, nil)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if len(ids) > 0 {
		wk.Relay.TryFlush(ctx, ids...)
	}
	return retErr
}

func (wk *Worker) HandleEmail(ctx context.Context, d pubsubx.Delivery) error {
	var msg messages.EmailRequested
	if err := json.Unmarshal(d.Data, &msg); err != nil {
		slog.ErrorContext(ctx, "bad email message; acking", "err", err)
		return nil
	}
	// The outbox id is the idempotency key. Messages published outside the
	// outbox fall back to the Pub/Sub message id.
	key, err := uuid.Parse(d.Attributes[messages.AttrOutboxID])
	if err != nil {
		key = uuid.NewSHA1(uuid.NameSpaceOID, []byte(d.MessageID))
	}
	log := slog.With("email_id", key, "template", msg.Template)

	var status string
	err = wk.Pool.QueryRow(ctx,
		`INSERT INTO email_deliveries (message_id, recipient, template) VALUES ($1, $2, $3)
		 ON CONFLICT (message_id) DO UPDATE SET message_id = EXCLUDED.message_id
		 RETURNING status`, key, msg.To, msg.Template).Scan(&status)
	if err != nil {
		return err
	}
	if status == "sent" {
		log.InfoContext(ctx, "email already sent; acking duplicate")
		return nil
	}

	m, err := mail.Render(msg.Template, msg.To, msg.Data)
	if err != nil {
		log.ErrorContext(ctx, "cannot render email; dropping", "err", err)
		return nil
	}

	// A crash between Send and the UPDATE below can cause one duplicate email.
	// That's the accepted trade-off of at-least-once delivery; use a provider
	// with idempotency keys if duplicates are unacceptable.
	if err := wk.Mailer.Send(ctx, m); err != nil {
		switch {
		case errors.Is(err, mail.ErrPermanent):
			log.ErrorContext(ctx, "permanent email failure; dropping", "err", err)
			return nil
		case errors.Is(err, mail.ErrRateLimited):
			return errRetryLater
		default:
			return err
		}
	}
	if _, err := wk.Pool.Exec(context.WithoutCancel(ctx),
		`UPDATE email_deliveries SET status = 'sent', sent_at = now() WHERE message_id = $1`, key); err != nil {
		log.ErrorContext(ctx, "email sent but not recorded", "err", err)
	}
	log.InfoContext(ctx, "email sent")
	return nil
}

// SweepOutbox publishes outbox rows the fast path missed, every interval,
// until ctx is cancelled. Used on GKE, where pods run continuously. Safe to
// run in every replica: rows are claimed with FOR UPDATE SKIP LOCKED.
func (wk *Worker) SweepOutbox(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := wk.Relay.Flush(ctx)
			if err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "outbox sweep", "err", err, "published", n)
			} else if n > 0 {
				slog.InfoContext(ctx, "outbox sweep published backlog", "published", n)
			}
		}
	}
}

// relayOutbox is called by Cloud Scheduler (Cloud Run deployments) to publish
// any outbox rows the fast path missed.
func (wk *Worker) relayOutbox(w http.ResponseWriter, r *http.Request) {
	n, err := wk.Relay.Flush(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "outbox relay", "err", err, "published", n)
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n > 0 {
		slog.InfoContext(r.Context(), "outbox relay published backlog", "published", n)
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"published": n})
}
