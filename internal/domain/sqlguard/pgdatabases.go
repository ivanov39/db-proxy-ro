package sqlguard

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// PgDatabases describes what a PostgreSQL client should see in place of the
// server's real list of databases.
type PgDatabases struct {
	Names   []string // databases offered by the proxy
	Current string   // the name the client connected with
	Real    string   // the real name of that database on the server
}

// Virtualize rewrites a query so that pg_database lists the proxy's
// databases and current_database() returns the name the client connected
// with. This is what makes psql's \l and the database trees of GUI clients
// show the proxy's databases. The second result reports whether anything
// was changed.
//
// Only the table reference is replaced, by a subquery with the same columns:
// every row is the current database's real row with the name (and, for the
// other databases, the OID) substituted. Whatever columns the client asks
// for therefore keep working. The query must already have passed Check.
func (v PgDatabases) Virtualize(sql string) (string, bool) {
	var edits []edit
	for i, mode := range lexModes(Postgres, sql) {
		toks, err := lex(sql, Postgres, mode)
		if err != nil {
			return sql, false
		}
		found := v.edits(toks)
		if i == 0 {
			edits = found
			continue
		}
		// The rewrite must not depend on server settings the proxy cannot see.
		if len(found) != len(edits) {
			return sql, false
		}
		for j := range found {
			if found[j] != edits[j] {
				return sql, false
			}
		}
	}
	if len(edits) == 0 {
		return sql, false
	}
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(sql[last:e.pos])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(sql[last:])
	return b.String(), true
}

type edit struct {
	pos, end int
	text     string
}

func isName(t token, name string) bool {
	return (t.kind == kindWord && t.text == strings.ToUpper(name)) || (t.kind == kindQuoted && t.text == name)
}

// clauseWords can follow a table reference; any other word there is an alias.
var clauseWords = map[string]bool{
	"WHERE": true, "ORDER": true, "GROUP": true, "HAVING": true, "LIMIT": true, "OFFSET": true,
	"JOIN": true, "INNER": true, "LEFT": true, "RIGHT": true, "FULL": true, "CROSS": true,
	"NATURAL": true, "ON": true, "USING": true, "UNION": true, "INTERSECT": true, "EXCEPT": true,
	"FETCH": true, "FOR": true, "WINDOW": true, "TABLESAMPLE": true,
}

func (v PgDatabases) edits(toks []token) []edit {
	var edits []edit
	tableRefs := 0
	for i := 0; i < len(toks); i++ {
		// An optional "pg_catalog." qualifier.
		first := i
		if isName(toks[i], "pg_catalog") && i+2 < len(toks) && toks[i+1].isPunct(".") {
			i += 2
		}
		qualified := first != i
		if !qualified && first > 0 && toks[first-1].isPunct(".") {
			continue // a column or a name in another schema
		}
		next := token{kind: kindPunct} // a harmless stand-in at the end of the query
		if i+1 < len(toks) {
			next = toks[i+1]
		}

		switch {
		case isName(toks[i], "pg_database"):
			var prev token
			if first > 0 {
				prev = toks[first-1]
			}
			inFrom := prev.isWord("FROM") || prev.isWord("JOIN") || prev.isPunct(",")
			if !inFrom || next.isPunct(".") || next.isPunct("(") {
				continue
			}
			text := v.subquery(systemColumns(toks))
			hasAlias := next.kind == kindQuoted || (next.kind == kindWord && !clauseWords[next.text])
			if !hasAlias {
				text += " AS pg_database"
			}
			edits = append(edits, edit{toks[first].pos, toks[i].end, text})
			tableRefs++

		case isName(toks[i], "current_database") && next.isPunct("(") &&
			i+2 < len(toks) && toks[i+2].isPunct(")") && v.Current != v.Real:
			edits = append(edits, edit{toks[first].pos, toks[i+2].end, "(" + literal(v.Current) + "::name)"})
			i += 2
		}
	}
	if tableRefs == 0 && v.Current == v.Real {
		return nil
	}
	return edits
}

// systemColumns returns the hidden columns of a table (xmin, ctid, ...) that
// a query mentions. A subquery has none of its own, so the replacement for
// pg_database must carry them explicitly; adding only the ones in use keeps
// "SELECT *" free of extra columns.
func systemColumns(toks []token) []string {
	var cols []string
	for _, name := range []string{"xmin", "xmax", "cmin", "cmax", "ctid", "tableoid"} {
		for _, t := range toks {
			if isName(t, name) {
				cols = append(cols, name)
				break
			}
		}
	}
	return cols
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// subquery builds the replacement for pg_database. json_populate_record
// overrides chosen fields of a row and keeps the rest, whatever columns the
// server version has.
func (v PgDatabases) subquery(systemColumns []string) string {
	extra := ""
	for _, c := range systemColumns {
		extra += ", _d." + c
	}
	rows := make([]string, len(v.Names))
	for i, name := range v.Names {
		// A stable made-up OID, outside the range real databases use.
		h := fnv.New32a()
		h.Write([]byte(name))
		rows[i] = fmt.Sprintf("(%s, %d)", literal(name), 3_000_000_000+h.Sum32()%1_000_000_000)
	}
	return fmt.Sprintf("(SELECT (pg_catalog.json_populate_record(_d, pg_catalog.json_build_object("+
		"'datname', _v.n, 'oid', CASE WHEN _v.n = %s THEN _d.oid::bigint ELSE _v.o END))).*%s "+
		"FROM pg_catalog.pg_database _d, (VALUES %s) _v(n, o) WHERE _d.datname = %s)",
		literal(v.Current), extra, strings.Join(rows, ", "), literal(v.Real))
}
