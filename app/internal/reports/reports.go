// Package reports holds the report model, its Postgres repository, and the
// data sources that generate report contents.
package reports

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusReady   Status = "ready"
	StatusFailed  Status = "failed"
)

// Kinds are the report types callers may request. Each maps to a query in
// the BigQuery source.
var Kinds = []string{"events_by_type", "daily_active_users", "raw_events"}

const dateLayout = "2006-01-02"

// ErrInvalid marks a request that can never succeed; it is not retried.
var ErrInvalid = errors.New("invalid report request")

var ErrNotFound = errors.New("report not found")

type Params struct {
	From string `json:"from"` // inclusive, YYYY-MM-DD (UTC)
	To   string `json:"to"`   // exclusive, YYYY-MM-DD (UTC)
}

func (p Params) Range() (from, to time.Time, err error) {
	from, err = time.Parse(dateLayout, p.From)
	if err != nil {
		return from, to, fmt.Errorf("%w: from must be YYYY-MM-DD", ErrInvalid)
	}
	to, err = time.Parse(dateLayout, p.To)
	if err != nil {
		return from, to, fmt.Errorf("%w: to must be YYYY-MM-DD", ErrInvalid)
	}
	if !to.After(from) {
		return from, to, fmt.Errorf("%w: to must be after from", ErrInvalid)
	}
	if to.Sub(from) > 366*24*time.Hour {
		return from, to, fmt.Errorf("%w: range must be at most 366 days", ErrInvalid)
	}
	return from, to, nil
}

func Validate(kind string, p Params) error {
	if !slices.Contains(Kinds, kind) {
		return fmt.Errorf("%w: unknown kind %q (valid: %v)", ErrInvalid, kind, Kinds)
	}
	_, _, err := p.Range()
	return err
}

type Report struct {
	ID          uuid.UUID  `json:"id"`
	Kind        string     `json:"kind"`
	Params      Params     `json:"params"`
	NotifyEmail *string    `json:"notify_email,omitempty"`
	Status      Status     `json:"status"`
	FileKey     *string    `json:"-"`
	RowCount    *int64     `json:"row_count,omitempty"`
	Error       *string    `json:"error,omitempty"`
	Attempts    int        `json:"attempts"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	OwnerUID    *string    `json:"-"`
	OwnerEmail  *string    `json:"owner_email,omitempty"`
}

// Owner identifies who requested a report.
type Owner struct {
	UID   string
	Email string
}

const cols = `id, kind, params, notify_email, status, file_key, row_count, error, attempts, created_at, started_at, completed_at, owner_uid, owner_email`

func scan(row pgx.Row) (Report, error) {
	var r Report
	err := row.Scan(&r.ID, &r.Kind, &r.Params, &r.NotifyEmail, &r.Status, &r.FileKey,
		&r.RowCount, &r.Error, &r.Attempts, &r.CreatedAt, &r.StartedAt, &r.CompletedAt, &r.OwnerUID, &r.OwnerEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

type Repo struct {
	Pool *pgxpool.Pool
}

func (r *Repo) Create(ctx context.Context, tx pgx.Tx, owner Owner, kind string, p Params, notifyEmail string) (Report, error) {
	return scan(tx.QueryRow(ctx,
		`INSERT INTO reports (kind, params, notify_email, owner_uid, owner_email)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+cols,
		kind, p, nullable(notifyEmail), nullable(owner.UID), nullable(owner.Email)))
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// OwnedBy reports whether uid owns the report.
func (r Report) OwnedBy(uid string) bool {
	return r.OwnerUID != nil && *r.OwnerUID == uid
}

func (r *Repo) Get(ctx context.Context, id uuid.UUID) (Report, error) {
	return scan(r.Pool.QueryRow(ctx, `SELECT `+cols+` FROM reports WHERE id = $1`, id))
}

// List returns the most recent reports, newest first. An empty ownerUID
// lists everyone's reports.
func (r *Repo) List(ctx context.Context, ownerUID string, limit int) ([]Report, error) {
	rows, err := r.Pool.Query(ctx,
		`SELECT `+cols+` FROM reports WHERE ($1 = '' OR owner_uid = $1) ORDER BY created_at DESC LIMIT $2`,
		ownerUID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Report, error) { return scan(row) })
}

// Claim moves a report to running if it is pending, or if a previous run looks
// abandoned (running for longer than staleAfter). claimed=false returns the
// current state so the caller can decide whether to ack or retry later.
func (r *Repo) Claim(ctx context.Context, id uuid.UUID, staleAfter time.Duration) (rep Report, claimed bool, err error) {
	rep, err = scan(r.Pool.QueryRow(ctx,
		`UPDATE reports SET status = 'running', started_at = now(), attempts = attempts + 1, error = NULL
		 WHERE id = $1 AND (status = 'pending' OR (status = 'running' AND started_at < now() - make_interval(secs => $2)))
		 RETURNING `+cols, id, staleAfter.Seconds()))
	if err == nil {
		return rep, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return rep, false, err
	}
	rep, err = r.Get(ctx, id)
	return rep, false, err
}

// Release puts a running report back to pending after a retryable failure so
// the next delivery can claim it immediately.
func (r *Repo) Release(ctx context.Context, id uuid.UUID, cause string) error {
	_, err := r.Pool.Exec(ctx,
		`UPDATE reports SET status = 'pending', error = $2 WHERE id = $1 AND status = 'running'`, id, cause)
	return err
}

func (r *Repo) MarkReady(ctx context.Context, tx pgx.Tx, id uuid.UUID, key string, rows int64) error {
	_, err := tx.Exec(ctx,
		`UPDATE reports SET status = 'ready', file_key = $2, row_count = $3, error = NULL, completed_at = now()
		 WHERE id = $1`, id, key, rows)
	return err
}

func (r *Repo) MarkFailed(ctx context.Context, tx pgx.Tx, id uuid.UUID, cause string) error {
	_, err := tx.Exec(ctx,
		`UPDATE reports SET status = 'failed', error = $2, completed_at = now() WHERE id = $1`, id, cause)
	return err
}
