package domain

import "errors"

var (
	// ErrInvalidTarget is returned when a target definition is incomplete or malformed.
	ErrInvalidTarget = errors.New("invalid target")
	// ErrDuplicateDatabase is returned when two targets of one DBMS share a name.
	ErrDuplicateDatabase = errors.New("duplicate database name")
	// ErrUnknownDatabase is returned when a client asks for a database that is not configured.
	ErrUnknownDatabase = errors.New("unknown database")
)
