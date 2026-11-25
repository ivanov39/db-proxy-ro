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
	extra       string // extra TOML lines; `real = "x"` sets the real database name
}

var backends = []backend{
	{"pgsql", "pg16", 55432, pgPassword, ""},
	{"pgsql", "pg13", 55433, pgPassword, ""},
	{"pgsql", "pg16tls", 55432, pgPassword, `sslmode = "disable"`},
	{"mysql", "my80", 53306, myPassword, ""},
	{"mysql", "my80plain", 53306, myPassword, `sslmode = "disable"`},
	{"mysql", "my57", 53308, myPassword, ""},
	{"mysql", "maria", 53307, myPassword, ""},
	{"mysql", "mariaonly", 53307, myPassword, `real = "mariaonly"`},
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
		name, extra := "appdb", b.extra
		if real, ok := strings.CutPrefix(extra, "real = "); ok {
			name, extra = strings.Trim(real, `"`), ""
		}
		body := fmt.Sprintf("host = \"127.0.0.1\"\nport = %d\nuser = \"app\"\npassword = \"%s\"\nname = \"%s\"\nalias = \"%s\"\n%s\n",
			b.port, b.password, name, b.alias, extra)
		if err := os.WriteFile(filepath.Join(sub, b.alias+".toml"), []byte(body), 0o600); err != nil {
			panic(err)
		}
	}
	a, err := app.New(config.Config{
		ParamsDir: dir, PgSQLListen: "127.0.0.1:0", MySQLListen: "127.0.0.1:0",
		MySQLServerVersion: "8.0.36-test", MySQLAllDatabases: true, ConnectTimeout: 10 * time.Second, ShutdownTimeout: time.Second,
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

// ---------- SHOW DATABASES ----------

// psqlListQuery is what psql 16 sends for \l.
const psqlListQuery = `SELECT
  d.datname as "Name",
  pg_catalog.pg_get_userbyid(d.datdba) as "Owner",
  pg_catalog.pg_encoding_to_char(d.encoding) as "Encoding",
  d.datcollate as "Collate",
  d.datctype as "Ctype",
  CASE WHEN pg_catalog.array_length(d.datacl, 1) = 0
    THEN '(none)'
    ELSE pg_catalog.array_to_string(d.datacl, E'\\n')
  END as "Access privileges"
FROM pg_catalog.pg_database d
ORDER BY 1;`

// PostgreSQL clients list databases by reading pg_database. Through the
// proxy that table must show the proxy's databases.
func TestPostgresDatabaseList(t *testing.T) {
	const all = "pg13,pg16,pg16tls"
	for _, db := range []string{"pg16", "pg13"} {
		t.Run(db, func(t *testing.T) {
			conn := pgConnect(t, db)
			c := ctx(t)
			names := func(q string, mode pgx.QueryExecMode, args ...any) string {
				t.Helper()
				rows, err := conn.Query(c, q, append([]any{mode}, args...)...)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				defer rows.Close()
				var out []string
				for rows.Next() {
					vals, err := rows.Values()
					if err != nil {
						t.Fatalf("%s: %v", q, err)
					}
					out = append(out, fmt.Sprint(vals[0]))
				}
				if rows.Err() != nil {
					t.Fatalf("%s: %v", q, rows.Err())
				}
				return strings.Join(out, ",")
			}
			for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeSimpleProtocol, pgx.QueryExecModeCacheStatement} {
				for _, q := range []string{
					psqlListQuery,
					"SELECT datname FROM pg_database ORDER BY datname",
					"select datname from pg_catalog.pg_database where not datistemplate and datallowconn order by 1",
					`SELECT "pg_database".datname FROM "pg_catalog"."pg_database" ORDER BY pg_database.oid = 0, 1`,
					"SELECT n.datname FROM pg_class c, pg_database AS n WHERE c.relname = 'pg_class' ORDER BY 1",
				} {
					if got := names(q, mode); got != all {
						t.Errorf("mode %v, %s\n got %q", mode, q, got)
					}
				}
				if got := names("SELECT datname FROM pg_database WHERE datname = $1", pgx.QueryExecModeCacheStatement, "pg13"); got != "pg13" {
					t.Errorf("with a parameter: %q", got)
				}
				// What a JetBrains IDE sends: hidden columns and a parameter.
				ide := "select T.oid as object_id from pg_catalog.pg_tablespace T where pg_catalog.age(T.xmin) <= pg_catalog.age($1::varchar::xid) " +
					"union all select T.oid as object_id from pg_catalog.pg_database T where pg_catalog.age(T.xmin) <= pg_catalog.age($2::varchar::xid)"
				if _, err := conn.Exec(c, ide, pgx.QueryExecModeCacheStatement, "3", "3"); err != nil {
					t.Errorf("query with xmin: %v", err)
				}
				// The client connected as db, not as the real "appdb".
				if got := names("SELECT current_database()", mode); got != db {
					t.Errorf("current_database() = %q", got)
				}
				if got := names("SELECT datname FROM pg_database WHERE datname = current_database()", mode); got != db {
					t.Errorf("current row: %q", got)
				}
				// OIDs must tell the databases apart.
				if got := names("SELECT count(DISTINCT oid) FROM pg_database", mode); got != "3" {
					t.Errorf("distinct oids: %s", got)
				}
			}
			// Other catalog tables are untouched.
			if got := names("SELECT count(*) FROM pg_catalog.pg_class WHERE relname = 'pg_database'", pgx.QueryExecModeSimpleProtocol); got != "1" {
				t.Errorf("pg_class: %s", got)
			}
			if n := pgRowCount(t, conn); n != 1000 {
				t.Fatalf("row count = %d", n)
			}
		})
	}

	_, err := pgx.Connect(ctx(t), fmt.Sprintf("postgres://u@%s/nope?sslmode=disable", pgAddr))
	if err == nil || !strings.Contains(err.Error(), "available: pg13, pg16, pg16tls") {
		t.Errorf("unknown database error does not list the databases: %v", err)
	}
}

func TestMySQLShowDatabasesAndUse(t *testing.T) {
	c := ctx(t)
	list := func(conn *sql.Conn, q string) string {
		t.Helper()
		rows, err := conn.QueryContext(c, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		return strings.Join(names, ",")
	}
	version := func(conn *sql.Conn) string {
		t.Helper()
		var v string
		if err := conn.QueryRowContext(c, "SELECT version()").Scan(&v); err != nil {
			t.Fatalf("version: %v", err)
		}
		return v
	}

	// No database in the DSN: the client lands on the default database
	// (the first by name) and can work at once, as GUI tools expect.
	lobby := myOpen(t, "", "interpolateParams=true")
	if v := version(lobby); !strings.Contains(v, "MariaDB") {
		t.Errorf("default database: version %s", v)
	}
	if got := list(lobby, "SHOW DATABASES"); got != "maria,mariaonly,my57,my80,my80plain" {
		t.Errorf("SHOW DATABASES = %q", got)
	}
	if got := list(lobby, "show schemas like 'MY8%'"); got != "my80,my80plain" {
		t.Errorf("LIKE = %q", got)
	}
	if _, err := lobby.ExecContext(c, "USE nope"); myCode(err) == 0 {
		t.Error("USE nope succeeded")
	}
	if _, err := lobby.ExecContext(c, "USE my57"); err != nil {
		t.Fatalf("USE my57: %v", err)
	}
	if v := version(lobby); !strings.HasPrefix(v, "5.7") {
		t.Errorf("after USE my57: version %s", v)
	}
	// Switching to a database on another server.
	if _, err := lobby.ExecContext(c, "USE `maria`"); err != nil {
		t.Fatalf("USE maria: %v", err)
	}
	if v := version(lobby); !strings.Contains(v, "MariaDB") {
		t.Errorf("after USE maria: version %s", v)
	}
	if _, err := lobby.ExecContext(c, "DELETE FROM t"); myCode(err) != 1792 {
		t.Errorf("write after USE: %v", err)
	}
	var one int
	if err := lobby.QueryRowContext(c, "SELECT sneaky_write()").Scan(&one); myCode(err) != 1792 {
		t.Errorf("the session attached by USE is not read-only: %v", err)
	}

	// A name outside the catalog is a schema of the current server; the
	// session's own name leads back even though the real name differs.
	conn := myOpen(t, "my80", "interpolateParams=true")
	if got := list(conn, "SHOW DATABASES"); got != "maria,mariaonly,my57,my80,my80plain" {
		t.Errorf("SHOW DATABASES in a session = %q", got)
	}
	for _, q := range []string{"USE information_schema", "USE my80"} {
		if _, err := conn.ExecContext(c, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if n := myRowCount(t, conn); n != 3 {
		t.Fatalf("row count = %d", n)
	}
	if _, err := conn.PrepareContext(c, "SHOW DATABASES"); myCode(err) != 1235 {
		t.Errorf("prepared SHOW DATABASES: %v", err)
	}
}

// A client that names no database (it then sends its user name, or
// "postgres") lands on the default database and can list the others.
func TestPostgresNoDatabase(t *testing.T) {
	for _, url := range []string{
		"postgres://proxy@%s/?sslmode=disable",
		"postgres://anyone:secret@%s/postgres?sslmode=disable",
	} {
		conn, err := pgx.Connect(ctx(t), fmt.Sprintf(url, pgAddr))
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		var current string
		var count int
		err = conn.QueryRow(ctx(t), "SELECT current_database(), (SELECT count(*) FROM pg_database)").Scan(&current, &count)
		conn.Close(context.Background())
		if err != nil || current != "pg13" || count != 3 {
			t.Errorf("%s: current=%q, databases=%d, %v", url, current, count, err)
		}
	}
	// A wrong name is still an error.
	_, err := pgx.Connect(ctx(t), fmt.Sprintf("postgres://proxy@%s/typo?sslmode=disable", pgAddr))
	if pgCode(err) != "3D000" {
		t.Errorf("unknown database: %v", err)
	}
}

// One MySQL session sees every database of the proxy: information_schema
// covers all of them and other_db.table reaches another backend.
func TestMySQLAllDatabases(t *testing.T) {
	conn := myOpen(t, "my80", "interpolateParams=true")
	c := ctx(t)
	scan := func(q string) []string {
		t.Helper()
		rows, err := conn.QueryContext(c, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b sql.NullString
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, a.String+"/"+b.String)
		}
		return out
	}
	// Every backend holds a schema "appdb" with a table "t"; identical rows
	// are merged, while each server reports its own version.
	schemas := scan("/* ide */ -- List Schemas\nselect schema_name, 'x' from information_schema.schemata")
	appdb := 0
	for _, s := range schemas {
		if s == "appdb/x" {
			appdb++
		}
	}
	if appdb != 1 {
		t.Errorf("schemata: %v", schemas)
	}
	versions := scan("select distinct @@version, 'v' from information_schema.schemata")
	if len(versions) != 3 {
		t.Errorf("want rows from 3 servers, got %v", versions)
	}
	tables := scan("select table_schema, table_name from information_schema.tables where table_schema = 'appdb'")
	if len(tables) != 1 || tables[0] != "appdb/t" {
		t.Errorf("tables: %v", tables)
	}
	// The same schema named twice, as IDE queries do, is still one database.
	twice := scan("select table_schema, table_name from information_schema.tables where table_schema = 'mariaonly' " +
		"union all select table_schema, table_name from information_schema.tables where table_schema = 'mariaonly' and table_name = 'mariaonly'")
	if len(twice) != 1 || twice[0] != "mariaonly/t2" {
		t.Errorf("schema named twice: %v", twice)
	}
	// An error of the session's own database is the answer.
	if _, err := conn.QueryContext(c, "select nope from information_schema.schemata"); myCode(err) != 1054 {
		t.Errorf("bad column: %v", err)
	}

	// A qualified name routes the query to the database that holds it.
	maria := scan("select version(), count(*) from mariaonly.t2")
	if len(maria) != 1 || !strings.Contains(maria[0], "MariaDB") || !strings.HasSuffix(maria[0], "/2") {
		t.Errorf("mariaonly.t2: %v", maria)
	}
	if own := scan("select version(), 'own' from t limit 1"); len(own) != 1 || !strings.HasPrefix(own[0], "8.0") {
		t.Errorf("after a routed query the session left its database: %v", own)
	}
	if only := scan("select table_schema, table_name from information_schema.tables where table_schema = 'mariaonly'"); len(only) != 1 || only[0] != "mariaonly/t2" {
		t.Errorf("tables of mariaonly: %v", only)
	}
	if _, err := conn.ExecContext(c, "DELETE FROM mariaonly.t2"); myCode(err) != 1792 {
		t.Errorf("write to another database: %v", err)
	}
	// USE with a real schema name of another backend switches to it.
	if _, err := conn.ExecContext(c, "USE mariaonly"); err != nil {
		t.Fatalf("USE mariaonly: %v", err)
	}
	if got := scan("select count(*), 'n' from t2"); len(got) != 1 || got[0] != "2/n" {
		t.Errorf("after USE mariaonly: %v", got)
	}
	if _, err := conn.ExecContext(c, "USE my80"); err != nil {
		t.Fatalf("USE my80: %v", err)
	}
	if n := myRowCount(t, conn); n != 3 {
		t.Fatalf("row count = %d", n)
	}
}
