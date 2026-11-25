package mysql

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
	"github.com/ivanov39/db-proxy-ro/internal/domain/proxycmd"
	"github.com/ivanov39/db-proxy-ro/internal/usecase"
)

// session relays one client connection to one backend connection.
type session struct {
	h     *Handler
	ctx   context.Context
	log   *slog.Logger
	info  usecase.Session
	hello clientHello

	client  *conn
	backend *conn
	target  domain.Target
	// secondaries are connections to the other databases of the proxy,
	// opened on demand when AllDatabases is on. Keyed by catalog name.
	secondaries map[string]*conn

	statements map[uint32]string // prepared statement id -> SQL
	head       [headLen]byte     // first bytes of the packet being relayed
}

// result summarizes a backend reply for the query log.
type result struct {
	rows   int64
	err    string
	status uint16 // server status flags of the closing EOF
}

// run serves commands until the client quits or a connection breaks.
func (s *session) run() error {
	for {
		s.client.seq = 0
		cmd, err := s.client.readPacket(maxCommandLen)
		if err != nil {
			return err
		}
		if len(cmd) == 0 {
			return errors.New("empty command packet")
		}
		if cmd[0] == comQuit {
			s.backend.seq = 0
			return s.backend.send(cmd)
		}
		handled, err := s.proxyCommand(cmd)
		if err != nil {
			return err
		}
		if !handled {
			err = s.dispatch(cmd)
		}
		if err != nil {
			return err
		}
		if err := s.client.flush(); err != nil {
			return err
		}
	}
}

// close releases the backend connection, if any.
func (s *session) close() {
	if s.backend != nil {
		s.backend.Close()
	}
	for name, c := range s.secondaries {
		c.Close()
		delete(s.secondaries, name)
	}
}

// attach connects the session to a database of the catalog, replacing the
// current backend if there is one. It returns the server status flags, or
// the error to show to the client.
func (s *session) attach(name string) (uint16, *serverError) {
	log := s.log.With("db", name)
	target, ok := s.lookup(name)
	if !ok {
		log.Warn("unknown database requested")
		return 0, &serverError{1049, "42000", fmt.Sprintf("db-proxy-ro: database '%s' is not configured; available: %s",
			name, strings.Join(s.h.gate.Databases(domain.MySQL), ", "))}
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.h.opts.ConnectTimeout)
	backend, status, err := dialBackend(ctx, target, s.hello)
	cancel()
	if err != nil {
		log.Error("backend connection failed", "backend", target.Addr(), "error", err)
		var srvErr *serverError
		if errors.As(err, &srvErr) {
			return 0, &serverError{srvErr.Code, srvErr.State, "db-proxy-ro: backend refused the connection: " + srvErr.Message}
		}
		return 0, &serverError{2003, "HY000", "db-proxy-ro: cannot connect to the backend: " + err.Error()}
	}
	if s.backend != nil {
		s.backend.Close()
	}
	if c, ok := s.secondaries[name]; ok {
		c.Close()
		delete(s.secondaries, name)
	}
	s.backend = backend
	s.target = target
	s.info.Database = target.Name
	clear(s.statements)
	log.Debug("session attached", "backend", target.Addr())
	return status, nil
}

// proxyCommand handles what the proxy answers itself: SHOW DATABASES and
// USE with a database of the catalog. It reports whether the command was
// handled.
func (s *session) proxyCommand(cmd []byte) (bool, error) {
	var pc proxycmd.Command
	switch cmd[0] {
	case comQuery, comStmtPrepare:
		pc = proxycmd.Parse(string(cmd[1:]))
	case comInitDB:
		pc = proxycmd.Command{Kind: proxycmd.Use, Name: string(cmd[1:])}
	}
	sql := string(cmd[1:])

	switch {
	case pc.Kind == proxycmd.ShowDatabases && cmd[0] == comQuery:
		return true, s.replyDatabases(s.h.gate.ListDatabases(s.info, sql, pc))

	case pc.Kind == proxycmd.ShowDatabases || pc.Kind == proxycmd.Unsupported:
		s.h.gate.Refuse(s.info, "query", sql, proxycmd.UnsupportedReason)
		return true, s.reply(&serverError{1235, "42000", "db-proxy-ro: " + proxycmd.UnsupportedReason})

	case pc.Kind == proxycmd.Use && cmd[0] != comStmtPrepare &&
		(pc.Name == s.info.Database || pc.Name == s.target.Database):
		// Back to the session's own database, whose real name may differ
		// from the one the client knows.
		return true, s.dispatch(append([]byte{comInitDB}, s.target.Database...))

	case pc.Kind == proxycmd.Use && cmd[0] != comStmtPrepare:
		// A database of the catalog: switch backends. Any other name is a
		// schema of the current server and goes to it.
		if _, ok := s.lookup(pc.Name); !ok {
			return false, nil
		}
		start := time.Now()
		status, srvErr := s.attach(pc.Name)
		if srvErr != nil {
			s.h.gate.Refuse(s.info, "use", pc.Name, srvErr.Message)
			return true, s.reply(srvErr)
		}
		s.h.gate.Completed(s.info, "use", pc.Name, usecase.Outcome{Duration: time.Since(start)})
		return true, s.client.writePacket(okPacket(status))
	}

	return false, nil
}

// replyDatabases sends the result of SHOW DATABASES: a result set of one
// text column made by the proxy.
func (s *session) replyDatabases(values []string) error {
	const name = "Database"
	lenencString := func(b []byte, v string) []byte {
		return append(appendLenenc(b, uint64(len(v))), v...)
	}
	column := lenencString(nil, "def")
	column = lenencString(column, "") // schema
	column = lenencString(column, "") // table
	column = lenencString(column, "") // original table
	column = lenencString(column, name)
	column = lenencString(column, "")            // original name
	column = append(column, 0x0c, 45, 0)         // fixed-length part follows; utf8mb4
	column = appendUint32(column, 256)           // column length
	column = append(column, 0xfd, 0, 0, 0, 0, 0) // VAR_STRING, no flags, no decimals

	eof := []byte{pktEOF, 0, 0, byte(statusAutocommit), byte(statusAutocommit >> 8)}
	packets := [][]byte{{1}, column, eof}
	for _, v := range values {
		packets = append(packets, lenencString(nil, v))
	}
	packets = append(packets, eof)
	for _, p := range packets {
		if err := s.client.writePacket(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) dispatch(cmd []byte) error {
	switch cmd[0] {
	case comQuery:
		sql := string(cmd[1:])
		if !s.inspect("query", sql) {
			return nil
		}
		if handled, err := s.crossDatabase(sql, cmd); handled {
			return err
		}
		return s.logged("query", sql, cmd, s.relayResults)

	case comStmtPrepare:
		sql := string(cmd[1:])
		if !s.inspect("prepare", sql) {
			return nil
		}
		return s.logged("prepare", sql, cmd, func() (result, error) { return s.relayPrepared(sql) })

	case comStmtExecute:
		sql := "<unknown prepared statement>"
		if len(cmd) >= 5 {
			if known, ok := s.statements[binary.LittleEndian.Uint32(cmd[1:])]; ok {
				sql = known
			}
		}
		return s.logged("execute", sql, cmd, s.relayResults)

	case comInitDB:
		return s.logged("init_db", string(cmd[1:]), cmd, s.relayOne)

	case comStmtFetch:
		return s.exchange(cmd, func() (result, error) { return s.relayRows(result{}) })

	case comFieldList:
		return s.exchange(cmd, func() (result, error) { return s.relayUntilEOF() })

	case comPing, comStatistics, comStmtReset, comSetOption:
		return s.exchange(cmd, s.relayOne)

	case comStmtClose:
		if len(cmd) >= 5 {
			delete(s.statements, binary.LittleEndian.Uint32(cmd[1:]))
		}
		return s.forward(cmd) // no reply

	case comStmtSendLong:
		return s.forward(cmd) // no reply

	case comResetConnection:
		return s.resetConnection(cmd)

	default:
		name, ok := commandNames[cmd[0]]
		if !ok {
			name = fmt.Sprintf("command 0x%02x", cmd[0])
		}
		const reason = "this protocol command is not allowed"
		s.h.gate.Refuse(s.info, "command", name, reason)
		return s.reply(&serverError{1047, "08S01", "db-proxy-ro: " + name + ": " + reason})
	}
}

// inspect asks the guard about a SQL text and answers the client itself if
// the text is refused.
func (s *session) inspect(kind, sql string) bool {
	dec := s.h.gate.Inspect(s.info, kind, sql)
	if !dec.Allowed {
		// 1792 = ER_CANT_EXECUTE_IN_READ_ONLY_TRANSACTION
		s.reply(&serverError{1792, "25006", "db-proxy-ro: " + dec.Reason})
	}
	return dec.Allowed
}

// reply sends a packet made by the proxy as the answer to the current command.
func (s *session) reply(e *serverError) error {
	return s.client.writePacket(e.packet())
}

func (s *session) forward(cmd []byte) error {
	s.backend.seq = 0
	return s.backend.send(cmd)
}

// exchange forwards a command and relays its reply.
func (s *session) exchange(cmd []byte, relay func() (result, error)) error {
	if err := s.forward(cmd); err != nil {
		return err
	}
	_, err := relay()
	return err
}

// logged is exchange plus a query log record.
func (s *session) logged(kind, sql string, cmd []byte, relay func() (result, error)) error {
	start := time.Now()
	if err := s.forward(cmd); err != nil {
		return err
	}
	res, err := relay()
	if err != nil {
		res.err = "connection lost: " + err.Error()
	}
	s.h.gate.Completed(s.info, kind, sql, usecase.Outcome{Duration: time.Since(start), Rows: res.rows, Error: res.err})
	return err
}

// relayPacket copies one logical packet from the backend to the client and
// returns its first bytes for inspection. Frames are passed on as they
// arrive, so a huge row is never held in memory as a whole.
func (s *session) relayPacket() ([]byte, error) {
	var head []byte
	for {
		payload, err := s.backend.readFrame()
		if err != nil {
			return nil, err
		}
		if head == nil {
			head = s.head[:copy(s.head[:], payload)]
		}
		s.client.seq = s.backend.seq - 1
		if err := s.client.writeFrame(payload); err != nil {
			return nil, err
		}
		if len(payload) < maxFrame {
			return head, nil
		}
	}
}

// relayOne relays a reply that is a single packet (OK, ERR, EOF or a string).
func (s *session) relayOne() (result, error) {
	p, err := s.relayPacket()
	if err != nil {
		return result{}, err
	}
	if isERR(p) {
		return result{err: parseERR(p).Error()}, nil
	}
	return result{}, nil
}

// relayResults relays the reply to COM_QUERY or COM_STMT_EXECUTE: one or
// more results, each either an OK packet or a result set.
func (s *session) relayResults() (result, error) {
	var res result
	for {
		p, err := s.relayPacket()
		if err != nil {
			return res, err
		}
		var status uint16
		switch {
		case len(p) == 0:
			return res, errors.New("empty packet from the backend")
		case p[0] == pktERR:
			res.err = parseERR(p).Error()
			return res, nil
		case p[0] == pktOK:
			status = okStatus(p)
		case p[0] == pktLocalInfile:
			// The guard refuses LOAD DATA, so the server has no reason to ask
			// for a client file. Do not play along.
			return res, errors.New("backend requested a local file: session terminated")
		default: // a result set: column count, columns, EOF, rows, EOF
			r := reader{b: p}
			columns := r.lenenc()
			for i := uint64(0); i < columns; i++ {
				if _, err := s.relayPacket(); err != nil {
					return res, err
				}
			}
			eof, err := s.relayPacket()
			if err != nil {
				return res, err
			}
			if isERR(eof) {
				res.err = parseERR(eof).Error()
				return res, nil
			}
			if status = eofStatus(eof); status&statusCursorExists != 0 {
				break // rows are fetched later with COM_STMT_FETCH
			}
			if res, err = s.relayRows(res); err != nil || res.err != "" {
				return res, err
			}
			status = res.status
		}
		if status&statusMoreResults == 0 {
			return res, nil
		}
	}
}

// relayRows relays row packets up to the closing EOF or ERR.
func (s *session) relayRows(res result) (result, error) {
	for {
		p, err := s.relayPacket()
		switch {
		case err != nil:
			return res, err
		case isERR(p):
			res.err = parseERR(p).Error()
			return res, nil
		case isEOF(p):
			res.status = eofStatus(p)
			return res, nil
		}
		res.rows++
	}
}

// relayUntilEOF relays the reply to COM_FIELD_LIST.
func (s *session) relayUntilEOF() (result, error) {
	res, err := s.relayRows(result{})
	res.rows = 0
	return res, err
}

// relayPrepared relays the reply to COM_STMT_PREPARE and remembers the SQL
// of the new statement for the query log.
func (s *session) relayPrepared(sql string) (result, error) {
	p, err := s.relayPacket()
	if err != nil {
		return result{}, err
	}
	if isERR(p) {
		return result{err: parseERR(p).Error()}, nil
	}
	if len(p) < 9 || p[0] != pktOK {
		return result{}, errors.New("malformed reply to COM_STMT_PREPARE")
	}
	s.statements[binary.LittleEndian.Uint32(p[1:])] = sql
	columns := binary.LittleEndian.Uint16(p[5:])
	params := binary.LittleEndian.Uint16(p[7:])
	// Each non-empty block of definitions is closed by an EOF packet.
	for _, n := range []uint16{params, columns} {
		if n == 0 {
			continue
		}
		for i := uint16(0); i <= n; i++ {
			if _, err := s.relayPacket(); err != nil {
				return result{}, err
			}
		}
	}
	return result{}, nil
}

// resetConnection handles COM_RESET_CONNECTION, which wipes the session
// state on the backend, read-only mode included. The mode is restored before
// the client learns that the reset is done.
func (s *session) resetConnection(cmd []byte) error {
	if err := s.forward(cmd); err != nil {
		return err
	}
	p, err := s.backend.readPacket(maxCommandLen)
	if err != nil {
		return err
	}
	reply := append([]byte(nil), p...)
	clear(s.statements)
	if !isERR(reply) {
		if _, err := setReadOnly(s.backend); err != nil {
			return err
		}
	}
	s.client.seq = 1
	return s.client.writePacket(reply)
}
