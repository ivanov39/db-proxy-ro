package usecase

import (
	"time"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
	"github.com/ivanov39/db-proxy-ro/internal/domain/proxycmd"
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

// Databases returns the names of the databases configured for a DBMS.
func (g *Gatekeeper) Databases(dbms domain.DBMS) []string {
	return g.catalog.Names(dbms)
}

// ListDatabases answers SHOW DATABASES: the proxy's own databases of the
// session's DBMS, not those of the backend server. The request is logged.
func (g *Gatekeeper) ListDatabases(s Session, sql string, cmd proxycmd.Command) []string {
	names := cmd.Filter(g.catalog.Names(s.DBMS))
	g.audit.Record(QueryRecord{Session: s, Kind: "proxy", SQL: sql, Allowed: true, Rows: int64(len(names))})
	return names
}

// PgCatalogView rewrites an allowed PostgreSQL query so that the server's
// list of databases (pg_database) shows the proxy's databases instead. This
// is the native way PostgreSQL clients list databases: psql's \l and the
// database trees of GUI tools. Queries that do not touch the list are
// returned unchanged.
func (g *Gatekeeper) PgCatalogView(s Session, sql string) string {
	target, err := g.catalog.Lookup(domain.PostgreSQL, s.Database)
	if err != nil {
		return sql
	}
	view := sqlguard.PgDatabases{
		Names:   g.catalog.Names(domain.PostgreSQL),
		Current: s.Database,
		Real:    target.Database,
	}
	rewritten, _ := view.Virtualize(sql)
	return rewritten
}

// Targets returns every database configured for a DBMS.
func (g *Gatekeeper) Targets(dbms domain.DBMS) []domain.Target {
	return g.catalog.Targets(dbms)
}
