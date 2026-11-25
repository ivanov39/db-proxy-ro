// Package domain holds the core entities and rules of the read-only proxy.
// It has no dependencies on transport, storage or logging details.
package domain

// DBMS identifies a supported database management system.
type DBMS string

const (
	PostgreSQL DBMS = "pgsql"
	MySQL      DBMS = "mysql"
)

// AllDBMS lists every supported DBMS in a stable order.
func AllDBMS() []DBMS { return []DBMS{PostgreSQL, MySQL} }

func (d DBMS) String() string { return string(d) }
