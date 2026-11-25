// Package logging builds the two logs of the proxy. Both are JSON lines:
// the service log (start-up, errors) goes to stderr, the query log to stdout.
// Where those streams end up is decided outside the process.
package logging

import (
	"io"
	"log/slog"

	"github.com/ivanov39/db-proxy-ro/internal/usecase"
)

// NewService returns the service (error) log.
func NewService(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// QueryLog writes the query log. It implements usecase.AuditLog.
type QueryLog struct {
	log       *slog.Logger
	sqlMaxLen int
}

// NewQueryLog returns a query log. SQL texts longer than sqlMaxLen bytes are
// truncated; zero means no limit.
func NewQueryLog(w io.Writer, sqlMaxLen int) *QueryLog {
	return &QueryLog{log: slog.New(slog.NewJSONHandler(w, nil)), sqlMaxLen: sqlMaxLen}
}

func (q *QueryLog) Record(r usecase.QueryRecord) {
	sql := r.SQL
	truncated := false
	if q.sqlMaxLen > 0 && len(sql) > q.sqlMaxLen {
		sql, truncated = sql[:q.sqlMaxLen], true
	}
	attrs := []any{
		slog.String("dbms", r.Session.DBMS.String()),
		slog.String("db", r.Session.Database),
		slog.Uint64("conn", r.Session.ID),
		slog.String("client", r.Session.ClientAddr),
		slog.String("user", r.Session.ClientUser),
		slog.String("kind", r.Kind),
		slog.String("sql", sql),
	}
	if truncated {
		attrs = append(attrs, slog.Int("sql_len", len(r.SQL)))
	}
	if !r.Allowed {
		q.log.Warn("rejected", append(attrs, slog.String("reason", r.Reason))...)
		return
	}
	attrs = append(attrs,
		slog.Float64("duration_ms", float64(r.Duration.Microseconds())/1000),
		slog.Int64("rows", r.Rows),
	)
	if r.Error != "" {
		attrs = append(attrs, slog.String("error", r.Error))
	}
	q.log.Info("query", attrs...)
}
