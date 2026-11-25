// Package usecase holds the application logic of the proxy: building the
// catalog of databases at start-up and deciding, for every client request,
// where it goes and whether it may pass.
//
// It depends only on the domain and on the ports declared here; transports
// (wire protocols) and infrastructure (files, logs) plug in from outside.
package usecase

import (
	"time"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
)

// TargetSource supplies database definitions (port).
type TargetSource interface {
	Load() ([]domain.Target, error)
}

// AuditLog receives one record per client request (port).
type AuditLog interface {
	Record(QueryRecord)
}

// Session identifies a client connection in audit records.
type Session struct {
	ID         uint64
	DBMS       domain.DBMS
	Database   string
	ClientAddr string
	ClientUser string
}

// QueryRecord is one entry of the query log.
type QueryRecord struct {
	Session  Session
	Kind     string // query, execute, prepare, command, ...
	SQL      string
	Allowed  bool
	Reason   string        // why the request was refused
	Duration time.Duration // time the backend took; zero for refused requests
	Rows     int64         // rows returned to the client
	Error    string        // error reported by the backend, if any
}
