package domain

import (
	"fmt"
	"net"
	"strconv"
)

// TLSMode controls how the proxy secures its connection to the real database.
type TLSMode string

const (
	TLSDisable    TLSMode = "disable"     // never use TLS
	TLSPrefer     TLSMode = "prefer"      // use TLS when the server offers it, do not verify (default)
	TLSRequire    TLSMode = "require"     // TLS is mandatory, certificate is not verified
	TLSVerifyFull TLSMode = "verify-full" // TLS is mandatory, certificate and host name are verified
)

// Target describes one real database the proxy can route clients to.
type Target struct {
	DBMS DBMS
	// Name is the database name a client asks for. It is unique per DBMS.
	Name string
	// Database is the real database name on the backend server.
	Database string
	Host     string
	Port     int
	User     string
	Password string
	TLS      TLSMode
	// Source tells where the definition came from (used in diagnostics only).
	Source string
}

// Addr returns the backend address in host:port form.
func (t Target) Addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// Validate checks that the target is complete and self-consistent.
func (t Target) Validate() error {
	switch {
	case t.Name == "":
		return fmt.Errorf("%w: database name is empty", ErrInvalidTarget)
	case t.Database == "":
		return fmt.Errorf("%w: backend database is empty", ErrInvalidTarget)
	case t.Host == "":
		return fmt.Errorf("%w: host is empty", ErrInvalidTarget)
	case t.User == "":
		return fmt.Errorf("%w: user is empty", ErrInvalidTarget)
	case t.Port < 1 || t.Port > 65535:
		return fmt.Errorf("%w: port %d is out of range 1..65535", ErrInvalidTarget, t.Port)
	}
	switch t.TLS {
	case TLSDisable, TLSPrefer, TLSRequire, TLSVerifyFull:
	default:
		return fmt.Errorf("%w: unknown sslmode %q (want disable, prefer, require or verify-full)", ErrInvalidTarget, t.TLS)
	}
	switch t.DBMS {
	case PostgreSQL, MySQL:
	default:
		return fmt.Errorf("%w: unknown DBMS %q", ErrInvalidTarget, t.DBMS)
	}
	return nil
}
