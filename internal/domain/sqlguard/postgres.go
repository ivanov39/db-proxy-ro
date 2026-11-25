package sqlguard

import "strings"

var pgRules = map[string]rule{
	"SELECT": nil, "WITH": nil, "TABLE": nil, "VALUES": nil,
	"SHOW": nil, "EXPLAIN": nil,
	"DECLARE": nil, "FETCH": nil, "MOVE": nil, "CLOSE": nil,
	"EXECUTE": nil, "DEALLOCATE": nil, "DISCARD": nil,
	"LISTEN": nil, "UNLISTEN": nil,
	"SAVEPOINT": nil, "RELEASE": nil, "END": nil, "ABORT": nil,

	"BEGIN":    noWrite,
	"START":    transactionStart,
	"SET":      noReadOnlySwitch,
	"RESET":    noReadOnlySwitch,
	"COMMIT":   pgNoTwoPhase,
	"ROLLBACK": pgNoTwoPhase,
	"PREPARE":  pgPrepare,
	"COPY":     pgCopy,
}

// pgDeniedWord reports keywords that turn an otherwise read-only statement
// into a writing one: data-modifying CTEs, SELECT INTO, row locks,
// EXPLAIN ANALYZE of DML or of CREATE TABLE AS, PREPARE of DML.
func pgDeniedWord(toks []token, i int) bool {
	switch toks[i].text {
	case "INSERT", "UPDATE", "DELETE", "MERGE", "INTO", "CREATE", "REFRESH":
		return true
	}
	return false
}

var pgDeniedFuncs = map[string]bool{
	// Would switch the session out of read-only mode.
	"set_config": true,
	// Affect other sessions or the server.
	"pg_terminate_backend": true, "pg_cancel_backend": true, "pg_reload_conf": true,
	"pg_rotate_logfile": true, "pg_promote": true, "pg_switch_wal": true,
	"pg_notify": true,
	// Sequences and large objects.
	"nextval": true, "setval": true,
	"lo_import": true, "lo_export": true, "lo_unlink": true, "lo_create": true, "lo_creat": true,
	"lo_put": true, "lo_from_bytea": true, "lo_truncate": true, "lo_truncate64": true,
	"lowrite": true, "lo_open": true,
	// Run SQL given as a string.
	"query_to_xml": true, "query_to_xmlschema": true, "query_to_xml_and_xmlschema": true,
	"cursor_to_xml": true, "cursor_to_xmlschema": true,
	"ts_stat": true, "ts_rewrite": true, "crosstab": true,
	// Server-side files.
	"pg_file_write": true, "pg_file_rename": true, "pg_file_unlink": true,
}

var pgDeniedFuncPrefixes = []string{
	"dblink", // runs SQL over a separate connection
	"pg_stat_reset", "pg_replication_origin", "pg_logical_", "pg_create_", "pg_drop_",
}

func pgDeniedFunc(name string) bool {
	if pgDeniedFuncs[name] {
		return true
	}
	for _, p := range pgDeniedFuncPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func pgNoTwoPhase(toks []token) Decision {
	if _, ok := hasWord(toks, "PREPARED"); ok {
		return deny("two-phase commit is not allowed")
	}
	return allowed
}

func pgPrepare(toks []token) Decision {
	if nextIsWord(toks, 0, "TRANSACTION") {
		return deny("two-phase commit is not allowed")
	}
	return allowed
}

// pgCopy allows only COPY ... TO STDOUT.
func pgCopy(toks []token) Decision {
	depth := 0
	toStdout := false
	for i, t := range toks {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case depth != 0 || t.kind != kindWord:
		case t.text == "FROM" || t.text == "PROGRAM":
			return deny("only COPY ... TO STDOUT is allowed")
		case t.text == "TO":
			if !nextIsWord(toks, i, "STDOUT") {
				return deny("only COPY ... TO STDOUT is allowed")
			}
			toStdout = true
		}
	}
	if !toStdout {
		return deny("only COPY ... TO STDOUT is allowed")
	}
	return allowed
}
