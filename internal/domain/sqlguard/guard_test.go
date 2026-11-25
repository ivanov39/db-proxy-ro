package sqlguard

import "testing"

type tc struct {
	sql  string
	want bool
}

func run(t *testing.T, d Dialect, cases []tc) {
	t.Helper()
	for _, c := range cases {
		got := Check(d, c.sql)
		if got.Allowed != c.want {
			t.Errorf("Check(%q) allowed=%v (reason %q), want %v", c.sql, got.Allowed, got.Reason, c.want)
		}
		if !got.Allowed && got.Reason == "" {
			t.Errorf("Check(%q): refused without a reason", c.sql)
		}
	}
}

func TestPostgres(t *testing.T) {
	run(t, Postgres, []tc{
		// Plain reads.
		{"SELECT 1", true},
		{"select * from users where name = 'delete' and note = 'insert into'", true},
		{"  -- leading comment\n SELECT 1;", true},
		{"/* outer /* nested */ still comment */ SELECT 1", true},
		{"(SELECT 1) UNION (SELECT 2)", true},
		{"WITH x AS (SELECT 1) SELECT * FROM x", true},
		{"TABLE users", true},
		{"VALUES (1), (2)", true},
		{"SHOW server_version", true},
		{"SHOW transaction_read_only", true},
		{"EXPLAIN ANALYZE SELECT * FROM t", true},
		{"SELECT t.update, t.delete FROM t", true},
		{`SELECT "update" FROM t`, true},
		{"SELECT replace(a, 'x', 'y') FROM t", true},
		{"SELECT $1::int, $2", true},
		{"SELECT $$it's; DELETE FROM t$$", true},
		{"SELECT $tag$ $$ ; DROP TABLE t $tag$", true},
		{"SELECT E'it\\'s fine'", true},
		{"", true},
		{";;", true},
		{"-- only a comment", true},
		{"SELECT 1; SELECT 2", true},
		// Sessions and transactions.
		{"BEGIN", true},
		{"BEGIN ISOLATION LEVEL REPEATABLE READ, READ ONLY", true},
		{"START TRANSACTION READ ONLY", true},
		{"COMMIT", true}, {"END", true}, {"ROLLBACK", true}, {"ROLLBACK TO SAVEPOINT a", true},
		{"SAVEPOINT a", true}, {"RELEASE SAVEPOINT a", true},
		{"SET search_path TO public", true},
		{"SET statement_timeout = '5s'", true},
		{"SET TRANSACTION ISOLATION LEVEL SERIALIZABLE", true},
		{"SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY", true},
		{"RESET ALL", true}, {"DISCARD ALL", true},
		{"DECLARE c CURSOR FOR SELECT 1", true}, {"FETCH 10 FROM c", true}, {"CLOSE c", true},
		{"PREPARE p AS SELECT $1::int", true}, {"EXECUTE p(1)", true}, {"DEALLOCATE p", true},
		{"COPY t TO STDOUT", true},
		{"COPY (SELECT * FROM t) TO STDOUT WITH (FORMAT csv)", true},

		// Writes and DDL.
		{"INSERT INTO t VALUES (1)", false},
		{"UPDATE t SET a = 1", false},
		{"DELETE FROM t", false},
		{"MERGE INTO t USING s ON true WHEN MATCHED THEN DELETE", false},
		{"TRUNCATE t", false},
		{"CREATE TABLE t (a int)", false},
		{"DROP TABLE t", false},
		{"ALTER TABLE t ADD b int", false},
		{"GRANT ALL ON t TO u", false},
		{"VACUUM", false}, {"ANALYZE t", false}, {"REINDEX TABLE t", false},
		{"CALL p()", false},
		{"DO $$ BEGIN DELETE FROM t; END $$", false},
		{"LOCK TABLE t", false},
		{"NOTIFY ch, 'x'", false},
		{"REFRESH MATERIALIZED VIEW v", false},
		{"COMMENT ON TABLE t IS 'x'", false},
		// Writes hidden inside reads.
		{"SELECT 1; DELETE FROM t", false},
		{"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", false},
		{"WITH d AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM d", false},
		{"SELECT * INTO backup FROM t", false},
		{"SELECT * FROM t FOR UPDATE", false},
		{"EXPLAIN ANALYZE DELETE FROM t", false},
		{"EXPLAIN (ANALYZE) CREATE TABLE x AS SELECT 1", false},
		{"PREPARE p AS DELETE FROM t", false},
		{"(((DELETE FROM t)))", false},
		{"SELECT nextval('s')", false},
		{"SELECT setval('s', 1)", false},
		{"SELECT pg_terminate_backend(1)", false},
		{"SELECT dblink_exec('c', 'delete from t')", false},
		{"SELECT query_to_xml('delete from t returning 1', true, true, '')", false},
		{"SELECT lo_unlink(1)", false},
		{"COPY t FROM STDIN", false},
		{"COPY t TO '/tmp/x'", false},
		{"COPY t TO PROGRAM 'sh'", false},
		{"COPY (SELECT 1) TO PROGRAM 'sh'", false},
		// Attempts to leave read-only mode.
		{"BEGIN READ WRITE", false},
		{"BEGIN ISOLATION LEVEL SERIALIZABLE, READ WRITE", false},
		{"START TRANSACTION READ WRITE", false},
		{"START SOMETHING", false},
		{"SET TRANSACTION READ WRITE", false},
		{"SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE", false},
		{"SET default_transaction_read_only = off", false},
		{"SET default_transaction_read_only TO off", false},
		{`SET "default_transaction_read_only" = off`, false},
		{"SET LOCAL transaction_read_only = off", false},
		{"RESET default_transaction_read_only", false},
		{"SELECT set_config('default_transaction_read_only', 'off', false)", false},
		{"SELECT pg_catalog.set_config('default_transaction_' || 'read_only', 'off', false)", false},
		{`SELECT "set_config"('a', 'b', false)`, false},
		{"SELECT set_config /* c */ ('a', 'b', false)", false},
		{`SELECT U&"set\005fconfig"('a', 'b', false)`, false},
		{"PREPARE TRANSACTION 'x'", false},
		{"COMMIT PREPARED 'x'", false},
		// Lexer tricks.
		{"SELECT 'a\\'; DELETE FROM t; --'", false}, // code when standard_conforming_strings=on
		{"SELECT 'a' /* /* */ ; DELETE FROM t", false},
		{"SELECT 1 /* unterminated", false},
		{"SELECT 'unterminated", false},
		{"SELECT $x$ unterminated", false},
		{"SELECT 1 --\rDELETE FROM t", false},
		{"/*! SELECT 1 */ DELETE FROM t", false},
		{"'SELECT'", false},
		{"123", false},
	})
}

func TestMySQL(t *testing.T) {
	run(t, MySQL, []tc{
		{"SELECT 1", true},
		{"select @@version_comment limit 1", true},
		{"SELECT * FROM t WHERE a = 'it''s' AND b = \"x\"", true},
		{"SELECT `update`, t.delete FROM t", true},
		{"SELECT REPLACE(a, 'x', 'y'), INSERT(a, 1, 2, 'z') FROM t", true},
		{"SELECT 1 # comment ; DELETE FROM t", true},
		{"SELECT 1 -- comment ; DELETE FROM t", true},
		{"SELECT /*+ MAX_EXECUTION_TIME(1000) */ 1", true},
		{"/* connector */ SELECT 1", true},
		{"SELECT 'a\\\\' , 1", true},
		{"WITH x AS (SELECT 1) SELECT * FROM x", true},
		{"SHOW CREATE TABLE t", true},
		{"SHOW FULL PROCESSLIST", true},
		{"SHOW VARIABLES LIKE 'transaction_read_only'", true},
		{"DESCRIBE t", true}, {"DESC t", true}, {"EXPLAIN SELECT 1", true},
		{"USE other", true}, {"HELP 'select'", true}, {"CHECKSUM TABLE t", true},
		{"SET NAMES utf8mb4", true},
		{"SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci", true},
		{"SET autocommit=1", true},
		{"SET SESSION sql_mode = 'ANSI_QUOTES,NO_BACKSLASH_ESCAPES'", true},
		{"SET @a = 1, @b := (SELECT COUNT(*) FROM t)", true},
		{"SET character_set_results = NULL", true},
		{"SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED", true},
		{"SET SESSION TRANSACTION READ ONLY", true},
		{"BEGIN", true}, {"BEGIN WORK", true},
		{"START TRANSACTION", true},
		{"START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY", true},
		{"COMMIT", true}, {"ROLLBACK", true}, {"SAVEPOINT a", true}, {"ROLLBACK TO SAVEPOINT a", true},
		{"", true},

		{"INSERT INTO t VALUES (1)", false},
		{"REPLACE INTO t VALUES (1)", false},
		{"UPDATE t SET a = 1", false},
		{"DELETE FROM t", false},
		{"TRUNCATE TABLE t", false},
		{"CREATE TABLE t (a int)", false}, {"DROP TABLE t", false}, {"ALTER TABLE t ADD b int", false},
		{"RENAME TABLE a TO b", false},
		{"LOAD DATA LOCAL INFILE 'x' INTO TABLE t", false},
		{"CALL p()", false}, {"DO SLEEP(1)", false},
		{"LOCK TABLES t WRITE", false}, {"UNLOCK TABLES", false},
		{"FLUSH TABLES", false}, {"KILL 1", false}, {"GRANT ALL ON *.* TO u", false},
		{"OPTIMIZE TABLE t", false}, {"ANALYZE TABLE t", false}, {"REPAIR TABLE t", false},
		{"HANDLER t OPEN", false},
		{"PREPARE s FROM 'DELETE FROM t'", false}, {"EXECUTE s", false},
		{"XA START 'x'", false},
		{"START SLAVE", false}, {"RESET MASTER", false}, {"SHUTDOWN", false},
		{"INSTALL PLUGIN x SONAME 'y'", false},
		// Writes hidden inside reads.
		{"SELECT 1; DELETE FROM t", false},
		{"WITH x AS (SELECT 1) DELETE FROM t", false},
		{"WITH x AS (SELECT 1) UPDATE t SET a = 1", false},
		{"SELECT * FROM t INTO OUTFILE '/tmp/x'", false},
		{"SELECT a INTO @v FROM t", false},
		{"SELECT * FROM t FOR UPDATE", false},
		{"SELECT * FROM t FOR SHARE", false},
		{"SELECT * FROM t LOCK IN SHARE MODE", false},
		{"SELECT GET_LOCK('x', 1)", false},
		{"SELECT `get_lock`('x', 1)", false},
		{"SELECT NEXT VALUE FOR s", false},
		{"EXPLAIN DELETE FROM t", false},
		{"BEGIN NOT ATOMIC DELETE FROM t; END", false},
		// Attempts to leave read-only mode.
		{"SET SESSION TRANSACTION READ WRITE", false},
		{"SET TRANSACTION READ WRITE", false},
		{"START TRANSACTION READ WRITE", false},
		{"SET transaction_read_only = 0", false},
		{"SET @@session.transaction_read_only = OFF", false},
		{"SET SESSION tx_read_only = 0", false},
		{"SET autocommit = 1, @@transaction_read_only = 0", false},
		{"SET GLOBAL max_connections = 1", false},
		{"SET @@global.max_connections = 1", false},
		{"SET PERSIST max_connections = 1", false},
		{"SET PASSWORD = 'x'", false},
		// Lexer tricks.
		{"/*!50000 DELETE FROM t */", false},
		{"SELECT 1 /*!99999 ' */ ; DELETE FROM t /*!99999 ' */", false},
		{"SELECT 1 /*M!100000 ; DELETE FROM t */", false},
		{"SELECT 'a\\'; DELETE FROM t; -- '", false}, // code under NO_BACKSLASH_ESCAPES
		{"SELECT \"a\\\"; DELETE FROM t; -- \"", false},
		{"SELECT 1--1 ; DELETE FROM t", false},
		{"SELECT 1 /* unterminated", false},
		{"SELECT 'unterminated", false},
		{"SELECT `unterminated", false},
		{"SELECT $$ ; DELETE FROM t $$", false}, // "$$" is not a quote in MySQL
	})
}

func FuzzCheck(f *testing.F) {
	for _, s := range []string{"SELECT 1", "select 'a\\'' ; delete from t", "/* x */ $a$ b $a$", "SET x = `y`"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		Check(Postgres, sql)
		Check(MySQL, sql)
	})
}
