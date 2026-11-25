package mysql

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
	"github.com/ivanov39/db-proxy-ro/internal/domain/sqlguard"
	"github.com/ivanov39/db-proxy-ro/internal/usecase"
)

// This file makes one MySQL session see every database of the proxy as if
// they were schemas of a single server, which is what GUI tools expect when
// they build their schema tree:
//
//   - a SELECT on information_schema is run on every database and the rows
//     are merged;
//   - a SELECT that names another database (other_db.table) is run on that
//     database.
//
// The extra backend connections are opened on first use, are read-only like
// the main one, and live until the session ends.

const (
	maxParallelDials = 8
	maxMergedBytes   = 256 << 20
)

// lookup finds a database of the catalog by the name clients use or, failing
// that, by its real name on the server, provided that is unambiguous.
func (s *session) lookup(name string) (domain.Target, bool) {
	if t, err := s.h.gate.Resolve(domain.MySQL, name); err == nil {
		return t, true
	}
	var found domain.Target
	matches := 0
	for _, t := range s.h.gate.Targets(domain.MySQL) {
		if t.Database == name {
			found = t
			matches++
		}
	}
	return found, matches == 1
}

// crossDatabase runs a query that concerns databases other than the
// session's own. It reports whether it handled the query.
func (s *session) crossDatabase(sql string, cmd []byte) (bool, error) {
	if !s.h.opts.AllDatabases {
		return false, nil
	}
	scope, ok := sqlguard.ScopeMySQL(sql)
	if !ok {
		return false, nil
	}

	if scope.InformationSchema {
		// A schema named in the query narrows the search to its database.
		// Each database once: a connection serves one query at a time.
		var targets []domain.Target
		named := make(map[string]bool)
		for _, lit := range scope.Literals {
			if t, ok := s.lookup(lit); ok && !named[t.Name] {
				named[t.Name] = true
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			targets = s.h.gate.Targets(domain.MySQL)
		}
		if len(targets) == 1 && targets[0].Name == s.target.Name {
			return false, nil
		}
		return true, s.fanOut(sql, cmd, targets)
	}

	// Exactly one other database is named: the query belongs there.
	var other *domain.Target
	for _, q := range scope.Qualifiers {
		// The query reaches the server as written, so only a real schema
		// name can be routed, not a name that exists in the proxy alone.
		t, ok := s.lookup(q)
		switch {
		case !ok || t.Database != q:
		case t.Name == s.target.Name:
			return false, nil // mixes in the current database: leave it alone
		case other != nil && other.Name != t.Name:
			return false, nil
		default:
			other = &t
		}
	}
	if other == nil {
		return false, nil
	}
	conns := s.connect([]domain.Target{*other})
	if conns[0] == nil {
		return true, s.reply(&serverError{2003, "HY000",
			"db-proxy-ro: cannot connect to the backend of database '" + other.Name + "'"})
	}
	main := s.backend
	s.backend = conns[0]
	err := s.logged("query", sql, cmd, s.relayResults)
	s.backend = main
	return true, err
}

// connect returns a connection for each target: the session's own for its
// current database, a secondary one otherwise. A nil entry means the
// database could not be reached.
func (s *session) connect(targets []domain.Target) []*conn {
	conns := make([]*conn, len(targets))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		limit = make(chan struct{}, maxParallelDials)
	)
	for i, t := range targets {
		if t.Name == s.target.Name {
			conns[i] = s.backend
			continue
		}
		if c, ok := s.secondaries[t.Name]; ok {
			conns[i] = c
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			ctx, cancel := context.WithTimeout(s.ctx, s.h.opts.ConnectTimeout)
			defer cancel()
			c, _, err := dialBackend(ctx, t, s.hello)
			if err != nil {
				s.log.Warn("cannot reach a database for a cross-database query", "db", t.Name, "error", err)
				return
			}
			mu.Lock()
			conns[i] = c
			s.secondaries[t.Name] = c
			mu.Unlock()
		}()
	}
	wg.Wait()
	return conns
}

// dropSecondary forgets a secondary connection that failed.
func (s *session) dropSecondary(c *conn) {
	for name, sc := range s.secondaries {
		if sc == c {
			c.Close()
			delete(s.secondaries, name)
		}
	}
}

// resultSet is a complete reply held in memory.
type resultSet struct {
	single  []byte   // an OK or ERR packet instead of a result set
	header  []byte   // column count
	columns [][]byte // column definitions
	rows    [][]byte
	eof     []byte
}

// query runs a command on one connection and reads the whole reply.
func query(c *conn, cmd []byte) (*resultSet, error) {
	c.seq = 0
	if err := c.send(cmd); err != nil {
		return nil, err
	}
	next := func() ([]byte, error) {
		p, err := c.readPacket(maxCommandLen)
		if err != nil {
			return nil, err
		}
		if len(p) == 0 {
			return nil, errors.New("empty packet from the backend")
		}
		return append([]byte(nil), p...), nil
	}
	first, err := next()
	if err != nil {
		return nil, err
	}
	switch first[0] {
	case pktOK, pktERR:
		return &resultSet{single: first}, nil
	case pktLocalInfile:
		return nil, errors.New("backend requested a local file")
	}
	rs := &resultSet{header: first}
	r := reader{b: first}
	for n := r.lenenc(); n > 0; n-- {
		col, err := next()
		if err != nil {
			return nil, err
		}
		rs.columns = append(rs.columns, col)
	}
	if rs.eof, err = next(); err != nil {
		return nil, err
	}
	if isERR(rs.eof) {
		return &resultSet{single: rs.eof}, nil
	}
	size := 0
	for {
		row, err := next()
		switch {
		case err != nil:
			return nil, err
		case isERR(row):
			return &resultSet{single: row}, nil
		case isEOF(row):
			if eofStatus(row)&statusMoreResults != 0 {
				return nil, errors.New("unexpected multiple results")
			}
			rs.eof = row
			return rs, nil
		}
		if size += len(row); size > maxMergedBytes {
			return nil, errors.New("result is too large to merge")
		}
		rs.rows = append(rs.rows, row)
	}
}

// fanOut runs a query on several databases and sends the client one result
// with the rows of all of them, without duplicates. The first reachable
// database is the reference: its columns are used and its error, if any, is
// the answer. Databases that fail are left out.
func (s *session) fanOut(sql string, cmd []byte, targets []domain.Target) error {
	start := time.Now()
	// The session's own database first: it is the reference.
	for i, t := range targets {
		if t.Name == s.target.Name {
			targets[0], targets[i] = targets[i], targets[0]
		}
	}
	conns := s.connect(targets)
	results := make([]*resultSet, len(conns))
	errs := make([]error, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		if c == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = query(c, cmd)
		}()
	}
	wg.Wait()

	var merged *resultSet
	seen := make(map[string]bool)
	for i, c := range conns {
		if c == nil {
			continue
		}
		if errs[i] != nil {
			if c == s.backend {
				return errs[i] // the session's own connection is broken
			}
			s.log.Warn("cross-database query failed on a database", "db", targets[i].Name, "error", errs[i])
			s.dropSecondary(c)
			continue
		}
		rs := results[i]
		if merged == nil {
			// The reference. A copy, so that rows can be appended.
			merged = &resultSet{single: rs.single, header: rs.header, columns: rs.columns, eof: rs.eof}
		}
		if merged.single != nil {
			break // the reference answered with OK or an error: that is the reply
		}
		if rs.single != nil || len(rs.columns) != len(merged.columns) {
			continue
		}
		for _, row := range rs.rows {
			if key := string(row); !seen[key] {
				seen[key] = true
				merged.rows = append(merged.rows, row)
			}
		}
	}
	if merged == nil {
		merged = &resultSet{single: (&serverError{2003, "HY000",
			"db-proxy-ro: none of the databases could answer the query"}).packet()}
	}

	outcome := usecase.Outcome{Duration: time.Since(start), Rows: int64(len(merged.rows))}
	packets := [][]byte{merged.single}
	if merged.single == nil {
		packets = append(append([][]byte{merged.header}, merged.columns...), merged.eof)
		packets = append(append(packets, merged.rows...), merged.eof)
	} else if isERR(merged.single) {
		outcome.Error = parseERR(merged.single).Error()
	}
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	s.log.Debug("cross-database query", "databases", strings.Join(names, ","), "rows", outcome.Rows)
	s.h.gate.Completed(s.info, "query", sql, outcome)
	for _, p := range packets {
		if err := s.client.writePacket(p); err != nil {
			return err
		}
	}
	return nil
}
