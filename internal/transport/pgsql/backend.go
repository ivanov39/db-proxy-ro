package pgsql

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
)

// readOnlySetting is the PostgreSQL setting that keeps a session read-only.
// It is passed in the start-up packet, which makes it the session default:
// RESET ALL and DISCARD ALL return to it rather than to the server default.
const readOnlySetting = "default_transaction_read_only"

// forwardedParams are the start-up parameters a client may set. Everything
// else is dropped: any setting can be passed this way, including the one
// that would turn read-only mode off.
var forwardedParams = map[string]bool{
	"application_name":   true,
	"client_encoding":    true,
	"datestyle":          true,
	"timezone":           true,
	"intervalstyle":      true,
	"extra_float_digits": true,
	"search_path":        true,
	"statement_timeout":  true,
}

// dialBackend opens an authenticated, read-only session to the real database
// and returns the raw connection, ready for message relaying.
func dialBackend(ctx context.Context, t domain.Target, clientParams map[string]string) (*pgconn.HijackedConn, error) {
	cfg, err := pgconn.ParseConfig(dsn(t))
	if err != nil {
		return nil, fmt.Errorf("build backend config: %w", err)
	}
	// Start from a clean slate: ParseConfig also merges PG* environment variables.
	cfg.RuntimeParams = map[string]string{readOnlySetting: "on"}
	for k, v := range clientParams {
		if forwardedParams[strings.ToLower(k)] {
			cfg.RuntimeParams[k] = v
		}
	}
	cfg.ValidateConnect = nil
	cfg.AfterConnect = nil

	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	hj, err := conn.Hijack()
	if err != nil {
		conn.Close(ctx)
		return nil, err
	}
	if err := verifyReadOnly(ctx, hj); err != nil {
		hj.Conn.Close()
		return nil, err
	}
	return hj, nil
}

// verifyReadOnly makes sure the backend really applied the read-only setting.
// A connection pooler between the proxy and the database may silently drop it.
func verifyReadOnly(ctx context.Context, hj *pgconn.HijackedConn) error {
	value, reported := hj.ParameterStatuses[readOnlySetting] // reported since PostgreSQL 14
	if !reported {
		if deadline, ok := ctx.Deadline(); ok {
			hj.Conn.SetDeadline(deadline)
			defer hj.Conn.SetDeadline(time.Time{})
		}
		var err error
		if value, err = show(hj.Conn, readOnlySetting); err != nil {
			return fmt.Errorf("verify read-only mode: %w", err)
		}
	}
	if value != "on" {
		return fmt.Errorf("backend did not enter read-only mode (%s=%q); "+
			"is there a connection pooler in front of the database?", readOnlySetting, value)
	}
	return nil
}

// show runs "SHOW name" over a raw, idle connection.
func show(conn net.Conn, name string) (string, error) {
	if err := writeFrame(conn, msgQuery, append([]byte("SHOW "+name), 0)); err != nil {
		return "", err
	}
	fr := frameReader{r: bufio.NewReaderSize(conn, 512), max: 1 << 16}
	var value string
	var failure error
	for {
		typ, body, err := fr.next()
		if err != nil {
			return "", err
		}
		switch typ {
		case msgDataRow:
			// int16 column count, then int32 length + bytes of the single column.
			if len(body) >= 6 {
				value = string(body[6:])
			}
		case msgErrorResponse:
			failure = errors.New(errorMessage(body))
		case msgReadyForQuery:
			return value, failure
		}
	}
}

func dsn(t domain.Target) string {
	quote := func(s string) string {
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
	}
	return strings.Join([]string{
		"host=" + quote(t.Host),
		"port=" + strconv.Itoa(t.Port),
		"user=" + quote(t.User),
		"password=" + quote(t.Password),
		"dbname=" + quote(t.Database),
		"sslmode=" + string(t.TLS), // the domain names match libpq's
	}, " ")
}

// sendCancel forwards a query cancellation to the backend.
func sendCancel(ctx context.Context, addr string, pid uint32, key []byte) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	msg := make([]byte, 0, 12+len(key))
	msg = appendUint32(msg, uint32(12+len(key)))
	msg = appendUint32(msg, codeCancelRequest)
	msg = appendUint32(msg, pid)
	msg = append(msg, key...)
	if _, err := conn.Write(msg); err != nil {
		return err
	}
	// The server closes the connection once it has handled the request.
	conn.Read(make([]byte, 1))
	return nil
}

func appendUint32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
