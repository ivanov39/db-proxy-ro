// Package mysql is the MySQL side of the proxy. To a client it looks like a
// MySQL server that accepts any credentials; behind it sits a real MySQL or
// MariaDB server reached with the credentials from the catalog.
//
// The proxy performs both handshakes itself and then relays packets
// unchanged, one command at a time: it reads a command, inspects it, forwards
// it and copies the complete reply back before reading the next command.
package mysql

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
	"github.com/ivanov39/db-proxy-ro/internal/usecase"
)

const handshakeTimeout = 30 * time.Second

// Options configure the MySQL side of the proxy.
type Options struct {
	ConnectTimeout time.Duration
	// ServerVersion is shown to clients in the greeting, which is sent
	// before the backend is known.
	ServerVersion string
	// DefaultDB is the database given to clients that name none.
	DefaultDB string
	// AllDatabases lets one session see every database of the proxy, as if
	// they were schemas of one server (see crossdb.go).
	AllDatabases bool
}

// Handler serves MySQL client connections.
type Handler struct {
	gate   *usecase.Gatekeeper
	log    *slog.Logger
	opts   Options
	nextID atomic.Uint64
}

func NewHandler(gate *usecase.Gatekeeper, log *slog.Logger, opts Options) *Handler {
	return &Handler{gate: gate, log: log, opts: opts}
}

// Serve runs one client connection from handshake to disconnect.
func (h *Handler) Serve(ctx context.Context, raw net.Conn) {
	info := usecase.Session{
		ID:         h.nextID.Add(1),
		DBMS:       domain.MySQL,
		ClientAddr: raw.RemoteAddr().String(),
	}
	log := h.log.With("dbms", "mysql", "conn", info.ID, "client", info.ClientAddr)
	client := newConn(raw)

	client.SetDeadline(time.Now().Add(handshakeTimeout))
	hello, err := h.handshake(client, uint32(info.ID))
	if err != nil {
		if !errors.Is(err, io.EOF) {
			log.Warn("client handshake failed", "error", err)
		}
		return
	}
	client.SetDeadline(time.Time{})

	info.ClientUser, info.Database = hello.user, hello.database

	s := &session{h: h, ctx: ctx, log: log, info: info, hello: hello, client: client,
		statements: make(map[uint32]string), secondaries: make(map[string]*conn)}
	defer s.close()

	// A client that names no database gets the default one. It can then
	// list the databases and switch with USE.
	name := info.Database
	if name == "" {
		name = h.opts.DefaultDB
	}
	if name == "" {
		client.send((&serverError{1046, "3D000", "db-proxy-ro: no MySQL databases are configured"}).packet())
		return
	}
	status, srvErr := s.attach(name)
	if srvErr != nil {
		client.send(srvErr.packet())
		return
	}
	if err := client.send(okPacket(status)); err != nil {
		return
	}
	if err := s.run(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		log.Warn("session ended with an error", "error", err)
	}
	log.Debug("session closed")
}

// handshake plays the server side of the connection phase. Whatever
// credentials the client presents are accepted.
func (h *Handler) handshake(client *conn, connID uint32) (clientHello, error) {
	// The salt must be 20 non-zero bytes; printable ones are the convention.
	salt := make([]byte, 20)
	rand.Read(salt)
	for i, b := range salt {
		salt[i] = '!' + b%('~'-'!')
	}

	caps := offeredCaps // a variable: the constant does not fit the byte conversions below
	g := []byte{10}
	g = append(append(g, h.opts.ServerVersion...), 0)
	g = appendUint32(g, connID)
	g = append(append(g, salt[:8]...), 0)
	g = append(g, byte(caps), byte(caps>>8))
	g = append(g, 45) // utf8mb4_general_ci, known to every client
	g = append(g, byte(statusAutocommit), byte(statusAutocommit>>8))
	g = append(g, byte(caps>>16), byte(caps>>24))
	g = append(g, byte(len(salt)+1))
	g = append(g, make([]byte, 10)...)
	g = append(append(g, salt[8:]...), 0)
	g = append(append(g, nativePassword...), 0)
	if err := client.send(g); err != nil {
		return clientHello{}, err
	}

	p, err := client.readPacket(maxCommandLen)
	if err != nil {
		return clientHello{}, err
	}
	hello, err := parseHandshakeResponse(p)
	if err != nil {
		client.send((&serverError{1043, "08S01", "db-proxy-ro: " + err.Error()}).packet())
		return clientHello{}, err
	}

	// Finish the client's authentication plugin so that it is ready for the
	// final OK. No password is checked.
	switch {
	case hello.plugin == "" || hello.plugin == nativePassword:
	case hello.plugin == cachingSHA2 && len(hello.auth) == 0:
	case hello.plugin == cachingSHA2:
		if err := client.send([]byte{pktAuthMore, 3}); err != nil { // "fast authentication succeeded"
			return clientHello{}, err
		}
	default:
		switchTo := append(append([]byte{pktEOF}, nativePassword...), 0)
		switchTo = append(append(switchTo, salt...), 0)
		if err := client.send(switchTo); err != nil {
			return clientHello{}, err
		}
		if _, err := client.readPacket(maxCommandLen); err != nil {
			return clientHello{}, err
		}
	}
	return hello, nil
}

func parseHandshakeResponse(p []byte) (clientHello, error) {
	var h clientHello
	r := reader{b: p}
	h.caps = r.uint32()
	if r.err() == nil && h.caps&capProtocol41 == 0 {
		return h, errors.New("clients older than MySQL 4.1 are not supported")
	}
	if h.caps&capSSL != 0 && len(p) == 32 {
		return h, errors.New("TLS is not offered by the proxy; disable it on the client")
	}
	h.caps &= offeredCaps
	h.maxPacket = r.uint32()
	h.charset = r.byte()
	r.bytes(23)
	h.user = r.cstring()
	switch {
	case h.caps&capPluginAuthLenenc != 0:
		h.auth = r.bytes(int(r.lenenc()))
	case h.caps&capSecureConnection != 0:
		h.auth = r.bytes(int(r.byte()))
	default:
		h.auth = []byte(r.cstring())
	}
	if h.caps&capConnectWithDB != 0 {
		h.database = r.cstring()
	}
	if h.caps&capPluginAuth != 0 && len(r.b) > 0 {
		h.plugin = r.cstring()
	}
	if err := r.err(); err != nil {
		return h, errors.New("malformed handshake response")
	}
	if h.caps&capConnectAttrs != 0 && len(r.b) > 0 {
		if attrs := r.bytes(int(r.lenenc())); r.err() == nil {
			h.attrs = append([]byte(nil), attrs...)
		}
	}
	h.auth = append([]byte(nil), h.auth...)
	return h, nil
}
