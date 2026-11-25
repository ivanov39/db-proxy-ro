//go:build integration

// Package integration runs the proxy against real database servers.
// Start them first: docker compose -f test/integration/docker-compose.yml up -d --wait
package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ivanov39/db-proxy-ro/internal/app"
	"github.com/ivanov39/db-proxy-ro/internal/infrastructure/config"
)

const (
	pgPassword = "s3cret'pass"
	myPassword = "s3cret-Pass#1"
)

type backend struct {
	dbms, alias string
	port        int
	password    string
	extra       string
}

var backends = []backend{
	{"pgsql", "pg16", 55432, pgPassword, ""},
	{"pgsql", "pg13", 55433, pgPassword, ""},
	{"pgsql", "pg16tls", 55432, pgPassword, `sslmode = "disable"`},
	{"mysql", "my80", 53306, myPassword, ""},
	{"mysql", "my80plain", 53306, myPassword, `sslmode = "disable"`},
	{"mysql", "my57", 53308, myPassword, ""},
	{"mysql", "maria", 53307, myPassword, ""},
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var (
	pgAddr, myAddr string
	queryLog       syncBuffer
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dbproxy-params")
	if err != nil {
		panic(err)
	}
	for _, b := range backends {
		sub := filepath.Join(dir, b.dbms)
		os.MkdirAll(sub, 0o755)
		body := fmt.Sprintf("host = \"127.0.0.1\"\nport = %d\nuser = \"app\"\npassword = \"%s\"\nname = \"appdb\"\nalias = \"%s\"\n%s\n",
			b.port, b.password, b.alias, b.extra)
		if err := os.WriteFile(filepath.Join(sub, b.alias+".toml"), []byte(body), 0o600); err != nil {
			panic(err)
		}
	}
	a, err := app.New(config.Config{
		ParamsDir: dir, PgSQLListen: "127.0.0.1:0", MySQLListen: "127.0.0.1:0",
		MySQLServerVersion: "8.0.36-test", ConnectTimeout: 10 * time.Second, ShutdownTimeout: time.Second,
	}, os.Stderr, &queryLog)
	if err != nil {
		panic(err)
	}
	pgAddr, myAddr = a.PgSQLAddr().String(), a.MySQLAddr().String()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	code := m.Run()
	stop()
	<-done
	os.RemoveAll(dir)
	os.Exit(code)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// ---------- PostgreSQL ----------

func pgConnect(t *testing.T, db string) *pgx.Conn {
	t.Helper()
	// Any user and password must do.
	conn, err := pgx.Connect(ctx(t), fmt.Sprintf("postgres://nobody:whatever@%s/%s?sslmode=disable", pgAddr, db))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func pgRowCount(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx(t), "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestPostgres(t *testing.T) {
	for _, db := range []string{"pg16", "pg13", "pg16tls"} {
		t.Run(db, func(t *testing.T) {
			conn := pgConnect(t, db)
			c := ctx(t)

			t.Run("read", func(t *testing.T) {
				var name string
				if err := conn.QueryRow(c, "SELECT name FROM t WHERE id = $1", 7).Scan(&name); err != nil || name != "row 7" {
					t.Fatalf("got %q, %v", name, err)
				}
				rows, err := conn.Query(c, "SELECT g, repeat('x', 100) FROM generate_series(1, 200000) g")
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for rows.Next() {
					n++
				}
				if rows.Err() != nil || n != 200000 {
					t.Fatalf("large result: %d rows, %v", n, rows.Err())
				}
			})

			t.Run("writes are refused", func(t *testing.T) {
				for _, q := range []string{
					"DELETE FROM t",
					"INSERT INTO t VALUES (5000, 'x')",
					"UPDATE t SET name = 'x'",
					"CREATE TABLE x (a int)",
					"TRUNCATE t",
					"WITH d AS (DELETE FROM t RETURNING *) SELECT count(*) FROM d",
					"SELECT nextval('s')",
					"BEGIN READ WRITE",
					"SET default_transaction_read_only = off",
				} {
					for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeSimpleProtocol, pgx.QueryExecModeCacheStatement, pgx.QueryExecModeExec} {
						_, err := conn.Exec(c, q, mode)
						if pgCode(err) != "25006" || !strings.Contains(err.Error(), "db-proxy-ro") {
							t.Errorf("%q (mode %v): want a proxy refusal, got %v", q, mode, err)
						}
					}
				}
				// Refusals must leave the connection usable and in sync.
				if n := pgRowCount(t, conn); n != 1000 {
					t.Fatalf("row count = %d", n)
				}
			})

			t.Run("write hidden in a function is refused by the backend", func(t *testing.T) {
				_, err := conn.Exec(c, "SELECT sneaky_write()")
				if pgCode(err) != "25006" || strings.Contains(err.Error(), "db-proxy-ro") {
					t.Fatalf("want a backend read-only error, got %v", err)
				}
			})

			t.Run("multi-statement simple query", func(t *testing.T) {
				_, err := conn.Exec(c, "SELECT 1; DELETE FROM t", pgx.QueryExecModeSimpleProtocol)
				if pgCode(err) != "25006" {
					t.Fatalf("got %v", err)
				}
				if _, err := conn.Exec(c, "SELECT 1; SELECT 2", pgx.QueryExecModeSimpleProtocol); err != nil {
					t.Fatal(err)
				}
			})

			t.Run("pipeline with a refused statement", func(t *testing.T) {
				batch := &pgx.Batch{}
				batch.Queue("SELECT 1")
				batch.Queue("DELETE FROM t")
				batch.Queue("SELECT 2")
				res := conn.SendBatch(c, batch)
				var one int
				err1 := res.QueryRow().Scan(&one)
				_, err2 := res.Exec()
				res.Close()
				if err1 == nil && one != 1 {
					t.Errorf("first statement returned %d", one)
				}
				if pgCode(err1) != "25006" && pgCode(err2) != "25006" {
					t.Errorf("want a refusal, got %v / %v", err1, err2)
				}
				if n := pgRowCount(t, conn); n != 1000 {
					t.Fatalf("row count = %d", n)
				}
			})

			t.Run("transactions", func(t *testing.T) {
				tx, err := conn.Begin(c)
				if err != nil {
					t.Fatal(err)
				}
				var n int
				if err := tx.QueryRow(c, "SELECT count(*) FROM t").Scan(&n); err != nil {
					t.Fatal(err)
				}
				// A refusal inside a transaction does not abort it.
				if _, err := tx.Exec(c, "DELETE FROM t"); pgCode(err) != "25006" {
					t.Fatalf("got %v", err)
				}
				if err := tx.QueryRow(c, "SELECT count(*) FROM t").Scan(&n); err != nil {
					t.Fatalf("transaction did not survive the refusal: %v", err)
				}
				if err := tx.Commit(c); err != nil {
					t.Fatal(err)
				}
				// DISCARD ALL resets the session, but not to read-write. It also
				// drops pgx's cached statements, hence the separate connection.
				other := pgConnect(t, db)
				if _, err := other.Exec(c, "DISCARD ALL", pgx.QueryExecModeSimpleProtocol); err != nil {
					t.Fatal(err)
				}
				_, err = other.Exec(c, "SELECT sneaky_write()", pgx.QueryExecModeSimpleProtocol)
				if pgCode(err) != "25006" {
					t.Fatalf("after DISCARD ALL: %v", err)
				}
			})

			t.Run("copy", func(t *testing.T) {
				var out bytes.Buffer
				tag, err := conn.PgConn().CopyTo(c, &out, "COPY t TO STDOUT")
				if err != nil || tag.RowsAffected() != 1000 || bytes.Count(out.Bytes(), []byte("\n")) != 1000 {
					t.Fatalf("COPY TO: %v, %v", tag, err)
				}
				_, err = conn.PgConn().CopyFrom(c, strings.NewReader("9\tx\n"), "COPY t FROM STDIN")
				if pgCode(err) != "25006" {
					t.Fatalf("COPY FROM: %v", err)
				}
				if n := pgRowCount(t, conn); n != 1000 {
					t.Fatalf("row count = %d", n)
				}
			})

			t.Run("cancel", func(t *testing.T) {
				short, cancel := context.WithTimeout(c, 300*time.Millisecond)
				defer cancel()
				start := time.Now()
				_, err := conn.Exec(short, "SELECT pg_sleep(20)")
				if err == nil || time.Since(start) > 5*time.Second {
					t.Fatalf("query was not cancelled: %v after %v", err, time.Since(start))
				}
				// pgx closes a connection whose context expired; the backend
				// query must have been cancelled too.
				other := pgConnect(t, db)
				deadline := time.Now().Add(5 * time.Second)
				for {
					var running int
					err := other.QueryRow(c, "SELECT count(*) FROM pg_stat_activity WHERE query = 'SELECT pg_sleep(20)' AND state = 'active'").Scan(&running)
					if err != nil {
						t.Fatal(err)
					}
					if running == 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the query is still running on the backend")
					}
					time.Sleep(100 * time.Millisecond)
				}
			})
		})
	}
}

// A function that turns read-only mode off passes the guard: the proxy
// cannot look inside it. On PostgreSQL 14+ the backend reports the change and
// the proxy must drop the session before anything can be written.
func TestPostgresWatchdog(t *testing.T) {
	conn := pgConnect(t, "pg16")
	c := ctx(t)
	_, err := conn.Exec(c, "SELECT sneaky_unlock()")
	if err == nil {
		_, err = conn.Exec(c, "SELECT sneaky_write()")
	}
	if err == nil {
		t.Fatal("the session survived leaving read-only mode")
	}
	if n := pgRowCount(t, pgConnect(t, "pg16")); n != 1000 {
		t.Fatalf("row count = %d: something was written", n)
	}
}

func TestPostgresUnknownDatabase(t *testing.T) {
	_, err := pgx.Connect(ctx(t), fmt.Sprintf("postgres://u@%s/nope?sslmode=disable", pgAddr))
	if pgCode(err) != "3D000" {
		t.Fatalf("got %v", err)
	}
}

// ---------- MySQL ----------

func myOpen(t *testing.T, db, params string) *sql.Conn {
	t.Helper()
	pool, err := sql.Open("mysql", fmt.Sprintf("nobody:whatever@tcp(%s)/%s?%s", myAddr, db, params))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	conn, err := pool.Conn(ctx(t))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func myCode(err error) uint16 {
	var myErr *mysqldrv.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number
	}
	return 0
}

func myRowCount(t *testing.T, conn *sql.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(ctx(t), "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestMySQL(t *testing.T) {
	for _, db := range []string{"my80", "my80plain", "my57", "maria"} {
		t.Run(db, func(t *testing.T) {
			c := ctx(t)
			// Text protocol and server-side prepared statements.
			for _, params := range []string{"interpolateParams=true", "interpolateParams=false"} {
				t.Run(params, func(t *testing.T) {
					conn := myOpen(t, db, params)

					var name string
					var null sql.NullString
					if err := conn.QueryRowContext(c, "SELECT name, NULL FROM t WHERE id = ?", 2).Scan(&name, &null); err != nil ||
						name != "row 2" || null.Valid {
						t.Fatalf("got %q, %v, %v", name, null, err)
					}

					for _, q := range []string{
						"DELETE FROM t",
						"INSERT INTO t VALUES (9, 'x')",
						"UPDATE t SET name = 'x'",
						"CREATE TABLE x (a int)",
						"SELECT * FROM t FOR UPDATE",
						"SET SESSION TRANSACTION READ WRITE",
						"START TRANSACTION READ WRITE",
						"CALL two_results()",
					} {
						_, err := conn.ExecContext(c, q)
						if myCode(err) != 1792 || !strings.Contains(err.Error(), "db-proxy-ro") {
							t.Errorf("%q: want a proxy refusal, got %v", q, err)
						}
					}
					// With a placeholder the statement goes through COM_STMT_PREPARE.
					if _, err := conn.ExecContext(c, "DELETE FROM t WHERE id = ?", 1); myCode(err) != 1792 {
						t.Errorf("prepared DELETE: %v", err)
					}

					var one int
					err := conn.QueryRowContext(c, "SELECT sneaky_write()").Scan(&one)
					if myCode(err) != 1792 || strings.Contains(err.Error(), "db-proxy-ro") {
						t.Errorf("want a backend read-only error, got %v", err)
					}

					tx, err := conn.BeginTx(c, &sql.TxOptions{ReadOnly: true})
					if err != nil {
						t.Fatal(err)
					}
					if err := tx.QueryRowContext(c, "SELECT 1").Scan(&one); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}

					if n := myRowCount(t, conn); n != 3 {
						t.Fatalf("row count = %d", n)
					}
				})
			}

			t.Run("multi-statements", func(t *testing.T) {
				conn := myOpen(t, db, "multiStatements=true")
				rows, err := conn.QueryContext(c, "SELECT 1; SELECT 2, 3")
				if err != nil {
					t.Fatal(err)
				}
				sets := 0
				for {
					for rows.Next() {
					}
					sets++
					if !rows.NextResultSet() {
						break
					}
				}
				rows.Close()
				if rows.Err() != nil || sets != 2 {
					t.Fatalf("%d result sets, %v", sets, rows.Err())
				}
				if _, err := conn.ExecContext(c, "SELECT 1; DELETE FROM t"); myCode(err) != 1792 {
					t.Fatalf("got %v", err)
				}
				if n := myRowCount(t, conn); n != 3 {
					t.Fatalf("row count = %d", n)
				}
			})

			t.Run("row larger than one frame", func(t *testing.T) {
				conn := myOpen(t, db, "maxAllowedPacket=0")
				if _, err := conn.ExecContext(c, "SET NAMES latin1"); err != nil {
					t.Fatal(err)
				}
				var big []byte
				err := conn.QueryRowContext(c, "SELECT repeat('a', 16777215 + 100)").Scan(&big)
				if myCode(err) == 1301 || (err == nil && big == nil) {
					t.Skip("the server's max_allowed_packet is too small")
				}
				if err != nil || len(big) != 16777215+100 {
					t.Fatalf("got %d bytes, %v", len(big), err)
				}
				if n := myRowCount(t, conn); n != 3 {
					t.Fatalf("row count = %d", n)
				}
			})
		})
	}
}

func TestMySQLUnknownDatabase(t *testing.T) {
	pool, _ := sql.Open("mysql", fmt.Sprintf("u@tcp(%s)/nope", myAddr))
	defer pool.Close()
	if err := pool.PingContext(ctx(t)); myCode(err) != 1049 {
		t.Fatalf("got %v", err)
	}
}

func TestQueryLog(t *testing.T) {
	conn := pgConnect(t, "pg16")
	conn.Exec(ctx(t), "SELECT 'marker-allowed'", pgx.QueryExecModeSimpleProtocol)
	conn.Exec(ctx(t), "DROP TABLE marker_refused", pgx.QueryExecModeSimpleProtocol)
	log := queryLog.String()
	for _, want := range []string{
		`"msg":"query","dbms":"pgsql","db":"pg16"`,
		`"sql":"SELECT 'marker-allowed'","duration_ms":`,
		`"sql":"DROP TABLE marker_refused","reason":"DROP statements are not allowed`,
		`"dbms":"mysql"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("query log lacks %s", want)
		}
	}
}
