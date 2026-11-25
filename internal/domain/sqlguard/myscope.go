package sqlguard

import "strings"

// MySQLScope tells which schemas a MySQL query refers to. The proxy uses it
// to let one session see every database it serves, as if they all lived on
// one server.
type MySQLScope struct {
	// InformationSchema: the query reads information_schema tables.
	InformationSchema bool
	// Qualifiers are the names used as a prefix (name.something), in order
	// of appearance and without duplicates. They include table aliases.
	Qualifiers []string
	// Literals are the string literals of the query.
	Literals []string
}

// ScopeMySQL analyses a single SELECT statement. It reports false for
// anything else, and for queries whose reading depends on the server's
// SQL mode.
func ScopeMySQL(sql string) (MySQLScope, bool) {
	var scope MySQLScope
	if strings.Contains(sql, `\`) {
		return scope, false
	}
	toks, err := lex(sql, MySQL, lexMode{})
	if err != nil {
		return scope, false
	}
	for len(toks) > 0 && toks[len(toks)-1].isPunct(";") {
		toks = toks[:len(toks)-1]
	}
	first := 0
	for first < len(toks) && toks[first].isPunct("(") {
		first++
	}
	if first == len(toks) || !(toks[first].isWord("SELECT") || toks[first].isWord("WITH")) {
		return scope, false
	}
	seen := make(map[string]bool)
	for i, t := range toks {
		switch {
		case t.isPunct(";"):
			return scope, false // several statements
		case t.kind == kindString:
			scope.Literals = append(scope.Literals, t.text)
		case (t.kind == kindWord || t.kind == kindQuoted) && i+1 < len(toks) && toks[i+1].isPunct(".") &&
			(i == 0 || !toks[i-1].isPunct(".")):
			name := t.text
			if t.kind == kindWord {
				name = sql[t.pos:t.end] // the token text is upper-cased
			}
			if strings.EqualFold(name, "information_schema") {
				scope.InformationSchema = true
			} else if !seen[name] {
				seen[name] = true
				scope.Qualifiers = append(scope.Qualifiers, name)
			}
		}
	}
	return scope, true
}
