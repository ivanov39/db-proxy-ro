// Package pgsql is the PostgreSQL side of the proxy. To a client it looks
// like a PostgreSQL server that accepts any credentials; behind it sits a
// real server reached with the credentials from the catalog.
//
// After the start-up phase, protocol messages are relayed unchanged in both
// directions. Only client messages that carry SQL are inspected.
package pgsql

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
	"github.com/ivanov39/db-proxy-ro/internal/usecase"
)

const startupTimeout = 30 * time.Second

// Handler serves PostgreSQL client connections.
type Handler struct {
	gate           *usecase.Gatekeeper
	log            *slog.Logger
	connectTimeout time.Duration
	nextID         atomic.Uint64

	// cancels maps the cancellation key given to a client to the backend it
	// belongs to. The proxy hands clients the backend's own key.
	mu      sync.Mutex
	cancels map[string]string
}

func NewHandler(gate *usecase.Gatekeeper, log *slog.Logger, connectTimeout time.Duration) *Handler {
	return &Handler{gate: gate, log: log, connectTimeout: connectTimeout, cancels: make(map[string]string)}
}

func cancelKey(pid uint32, key []byte) string {
	return string(appendUint32(nil, pid)) + string(key)
}

// Serve runs one client connection from start-up to disconnect.
func (h *Handler) Serve(ctx context.Context, client net.Conn) {
	info := usecase.Session{
		ID:         h.nextID.Add(1),
		DBMS:       domain.PostgreSQL,
		ClientAddr: client.RemoteAddr().String(),
	}
	log := h.log.With("dbms", "pgsql", "conn", info.ID, "client", info.ClientAddr)
	cr := bufio.NewReader(client)

	client.SetDeadline(time.Now().Add(startupTimeout))
	params, err := h.startup(ctx, client, cr)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			log.Warn("client start-up failed", "error", err)
		}
		return
	}
	if params == nil {
		return // a cancel request, already handled
	}
	client.SetDeadline(time.Time{})

	info.ClientUser = params["user"]
	info.Database = params["database"]
	if info.Database == "" {
		info.Database = info.ClientUser // PostgreSQL's own default
	}
	log = log.With("db", info.Database)

	if _, ok := params["replication"]; ok {
		fatal(client, "0A000", "db-proxy-ro: replication connections are not supported")
		log.Warn("replication connection refused")
		return
	}
	target, err := h.gate.Resolve(domain.PostgreSQL, info.Database)
	if err != nil {
		fatal(client, "3D000", fmt.Sprintf("db-proxy-ro: database %q is not configured", info.Database))
		log.Warn("unknown database requested")
		return
	}

	dialCtx, cancel := context.WithTimeout(ctx, h.connectTimeout)
	backend, err := dialBackend(dialCtx, target, params)
	cancel()
	if err != nil {
		log.Error("backend connection failed", "backend", target.Addr(), "error", err)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			fatal(client, pgErr.Code, "db-proxy-ro: backend refused the connection: "+pgErr.Message)
		} else {
			fatal(client, "08006", "db-proxy-ro: cannot connect to the backend: "+rootCause(err))
		}
		return
	}
	defer backend.Conn.Close()

	key := cancelKey(backend.PID, backend.SecretKey)
	h.mu.Lock()
	h.cancels[key] = target.Addr()
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.cancels, key)
		h.mu.Unlock()
	}()

	log.Debug("session opened", "backend", target.Addr())
	s := newSession(h.gate, log, info, client, cr, backend.Conn)
	if err := s.greet(backend); err == nil {
		err = s.run()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			log.Warn("session ended with an error", "error", err)
		}
	}
	log.Debug("session closed")
}

// startup reads start-up packets until the real one arrives and returns its
// parameters. It returns nil parameters after serving a cancel request.
func (h *Handler) startup(ctx context.Context, client net.Conn, cr *bufio.Reader) (map[string]string, error) {
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(cr, lenBuf[:]); err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint32(lenBuf[:])) - 4
		if n < 4 || n > maxStartupLen {
			return nil, fmt.Errorf("invalid start-up packet length %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(cr, body); err != nil {
			return nil, err
		}
		code := binary.BigEndian.Uint32(body)
		body = body[4:]

		switch {
		case code == codeSSLRequest || code == codeGSSEncRequest:
			// Encryption towards the client is not offered.
			if _, err := client.Write([]byte{'N'}); err != nil {
				return nil, err
			}
		case code == codeCancelRequest:
			if len(body) < 4 {
				return nil, errors.New("malformed cancel request")
			}
			h.cancel(ctx, binary.BigEndian.Uint32(body), body[4:])
			return nil, nil
		case code>>16 == 3:
			params := make(map[string]string)
			var unknown []string
			for len(body) > 1 {
				var k, v string
				k, body = cstring(body)
				v, body = cstring(body)
				if strings.HasPrefix(k, "_pq_.") {
					unknown = append(unknown, k)
					continue
				}
				params[k] = v
			}
			if code&0xFFFF != 0 || len(unknown) > 0 {
				// Ask the client to fall back to protocol 3.0.
				b := appendUint32(nil, 0)
				b = appendUint32(b, uint32(len(unknown)))
				for _, u := range unknown {
					b = append(append(b, u...), 0)
				}
				if err := writeFrame(client, msgNegotiateProtocolVn, b); err != nil {
					return nil, err
				}
			}
			return params, nil
		default:
			fatal(client, "0A000", fmt.Sprintf("db-proxy-ro: unsupported frontend protocol %d.%d", code>>16, code&0xFFFF))
			return nil, fmt.Errorf("unsupported protocol %d.%d", code>>16, code&0xFFFF)
		}
	}
}

func (h *Handler) cancel(ctx context.Context, pid uint32, key []byte) {
	h.mu.Lock()
	addr, ok := h.cancels[cancelKey(pid, key)]
	h.mu.Unlock()
	if !ok {
		return // same as PostgreSQL: unknown keys are ignored silently
	}
	ctx, done := context.WithTimeout(ctx, h.connectTimeout)
	defer done()
	if err := sendCancel(ctx, addr, pid, key); err != nil {
		h.log.Warn("cannot forward cancel request", "dbms", "pgsql", "backend", addr, "error", err)
	}
}

func fatal(w io.Writer, code, message string) {
	writeFrame(w, msgErrorResponse, errorBody("FATAL", code, message))
}

// rootCause strips the connection-string echo pgconn puts in front of errors.
func rootCause(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			// One line per attempt (with and without TLS); they rarely differ.
			line, _, _ := strings.Cut(err.Error(), "\n")
			return line
		}
		err = next
	}
}
