package reports

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// Source produces a report as CSV into w and returns the number of data rows.
type Source interface {
	Generate(ctx context.Context, kind string, p Params, w io.Writer) (int64, error)
}

// BigQuerySource runs whitelisted queries against the events table and streams
// results through the BigQuery Storage Read API, so memory stays flat no matter
// how large the result is.
type BigQuerySource struct {
	client   *bigquery.Client
	table    string // fully qualified, backtick-quoted
	location string
}

func NewBigQuerySource(ctx context.Context, project, dataset, table, location string) (*BigQuerySource, error) {
	c, err := bigquery.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("bigquery client: %w", err)
	}
	if err := c.EnableStorageReadClient(ctx); err != nil {
		return nil, fmt.Errorf("bigquery storage read client: %w", err)
	}
	return &BigQuerySource{
		client:   c,
		table:    fmt.Sprintf("`%s.%s.%s`", project, dataset, table),
		location: location,
	}, nil
}

func (s *BigQuerySource) Close() error { return s.client.Close() }

// Queries take @from (inclusive) and @to (exclusive) TIMESTAMP parameters.
// %[1]s is the events table. The table is partitioned on occurred_at, so the
// range filter prunes partitions and keeps cost proportional to the range.
var queries = map[string]string{
	"events_by_type": `
		SELECT event_type, COUNT(*) AS events, COUNT(DISTINCT user_id) AS users
		FROM %[1]s
		WHERE occurred_at >= @from AND occurred_at < @to
		GROUP BY event_type
		ORDER BY events DESC`,
	"daily_active_users": `
		SELECT DATE(occurred_at) AS day, COUNT(DISTINCT user_id) AS active_users, COUNT(*) AS events
		FROM %[1]s
		WHERE occurred_at >= @from AND occurred_at < @to
		GROUP BY day
		ORDER BY day`,
	"raw_events": `
		SELECT event_id, event_type, user_id, occurred_at, received_at, properties
		FROM %[1]s
		WHERE occurred_at >= @from AND occurred_at < @to
		ORDER BY occurred_at`,
}

func (s *BigQuerySource) Generate(ctx context.Context, kind string, p Params, w io.Writer) (int64, error) {
	tmpl, ok := queries[kind]
	if !ok {
		return 0, fmt.Errorf("%w: unknown kind %q", ErrInvalid, kind)
	}
	from, to, err := p.Range()
	if err != nil {
		return 0, err
	}

	q := s.client.Query(fmt.Sprintf(tmpl, s.table))
	q.Location = s.location
	q.Parameters = []bigquery.QueryParameter{
		{Name: "from", Value: from},
		{Name: "to", Value: to},
	}
	it, err := q.Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("run query: %w", err)
	}

	cw := csv.NewWriter(w)
	var n int64
	headerDone := false
	writeHeader := func() error {
		if headerDone || it.Schema == nil {
			return nil
		}
		headerDone = true
		h := make([]string, len(it.Schema))
		for i, f := range it.Schema {
			h[i] = f.Name
		}
		return cw.Write(h)
	}

	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return n, fmt.Errorf("read results: %w", err)
		}
		if err := writeHeader(); err != nil {
			return n, err
		}
		rec := make([]string, len(row))
		for i, v := range row {
			rec[i] = format(v)
		}
		if err := cw.Write(rec); err != nil {
			return n, err
		}
		n++
	}
	if err := writeHeader(); err != nil { // empty result still gets a header
		return n, err
	}
	cw.Flush()
	return n, cw.Error()
}

func format(v bigquery.Value) string {
	switch x := v.(type) {
	case nil:
		return ""
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// FakeSource writes synthetic rows. Used for local development where there is
// no BigQuery emulator.
type FakeSource struct {
	Delay time.Duration // simulate a slow query
}

func (f FakeSource) Generate(ctx context.Context, kind string, p Params, w io.Writer) (int64, error) {
	from, to, err := p.Range()
	if err != nil {
		return 0, err
	}
	if _, ok := queries[kind]; !ok {
		return 0, fmt.Errorf("%w: unknown kind %q", ErrInvalid, kind)
	}
	select {
	case <-time.After(f.Delay):
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	cw := csv.NewWriter(w)
	cw.Write([]string{"day", "active_users", "events"})
	var n int64
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		users := 100 + d.YearDay()%37
		cw.Write([]string{d.Format(dateLayout), strconv.Itoa(users), strconv.Itoa(users * 7)})
		n++
	}
	cw.Flush()
	return n, cw.Error()
}
