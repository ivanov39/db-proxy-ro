package usecase

import (
	"time"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
	"github.com/ivanov39/db-proxy-ro/internal/domain/sqlguard"
)

// Gatekeeper is what a transport asks about every client request.
type Gatekeeper struct {
	catalog *domain.Catalog
	audit   AuditLog
}

func NewGatekeeper(catalog *domain.Catalog, audit AuditLog) *Gatekeeper {
	return &Gatekeeper{catalog: catalog, audit: audit}
}

// Resolve finds the real database behind the name a client asked for.
func (g *Gatekeeper) Resolve(dbms domain.DBMS, name string) (domain.Target, error) {
	return g.catalog.Lookup(dbms, name)
}

// Inspect decides whether a SQL text may be sent to the backend. A refusal is
// written to the query log right away; an allowed request is logged by
// Completed once its outcome is known.
func (g *Gatekeeper) Inspect(s Session, kind, sql string) sqlguard.Decision {
	dialect := sqlguard.Postgres
	if s.DBMS == domain.MySQL {
		dialect = sqlguard.MySQL
	}
	dec := sqlguard.Check(dialect, sql)
	if !dec.Allowed {
		g.audit.Record(QueryRecord{Session: s, Kind: kind, SQL: sql, Reason: dec.Reason})
	}
	return dec
}

// Refuse logs a request that is refused for a reason other than its SQL text,
// such as a protocol command the proxy never forwards.
func (g *Gatekeeper) Refuse(s Session, kind, detail, reason string) {
	g.audit.Record(QueryRecord{Session: s, Kind: kind, SQL: detail, Reason: reason})
}

// Outcome is what happened to a request that was sent to the backend.
type Outcome struct {
	Duration time.Duration
	Rows     int64
	Error    string
}

// Completed logs a request that was sent to the backend.
func (g *Gatekeeper) Completed(s Session, kind, sql string, o Outcome) {
	g.audit.Record(QueryRecord{
		Session: s, Kind: kind, SQL: sql, Allowed: true,
		Duration: o.Duration, Rows: o.Rows, Error: o.Error,
	})
}
