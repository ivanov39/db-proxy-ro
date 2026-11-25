// Package sqlguard decides whether a SQL text is a read-only request.
//
// It is an allow-list: a statement passes only if it starts with a known
// read-only command and contains nothing from a deny-list of data-changing
// keywords and functions. Anything the guard does not understand is refused.
//
// The guard is one of two layers of protection. The transport layer also puts
// every backend session into read-only transaction mode, so a write hidden
// from the guard (for example inside a user-defined function) is still refused
// by the database itself. The guard in turn refuses every statement that could
// switch that mode off.
package sqlguard

import (
	"fmt"
	"strings"
)

// Decision is the result of inspecting a query.
type Decision struct {
	Allowed bool
	Reason  string // why the query was refused; empty when allowed
}

func deny(format string, args ...any) Decision {
	return Decision{Reason: fmt.Sprintf(format, args...)}
}

var allowed = Decision{Allowed: true}

// Check inspects a query text, which may hold several statements.
func Check(d Dialect, sql string) Decision {
	if d == MySQL && (strings.Contains(sql, "/*!") || strings.Contains(sql, "/*M!")) {
		// Whether such a comment is code depends on the server version.
		return deny("executable comments (/*! ... */) are not allowed")
	}
	for _, mode := range lexModes(d, sql) {
		toks, err := lex(sql, d, mode)
		if err != nil {
			return deny("cannot parse query: %v", err)
		}
		if dec := checkTokens(d, toks); !dec.Allowed {
			return dec
		}
	}
	return allowed
}

func lexModes(d Dialect, sql string) []lexMode {
	if !strings.Contains(sql, `\`) {
		return []lexMode{{}}
	}
	if d == Postgres {
		return []lexMode{{backslashSingle: false}, {backslashSingle: true}}
	}
	return []lexMode{{false, false}, {false, true}, {true, false}, {true, true}}
}

func checkTokens(d Dialect, toks []token) Decision {
	start := 0
	for i := 0; i <= len(toks); i++ {
		if i < len(toks) && !toks[i].isPunct(";") {
			continue
		}
		if stmt := toks[start:i]; len(stmt) > 0 {
			if dec := checkStatement(d, stmt); !dec.Allowed {
				return dec
			}
		}
		start = i + 1
	}
	return allowed
}

// rule performs the checks specific to one kind of statement. toks starts at
// the leading keyword.
type rule func(toks []token) Decision

func checkStatement(d Dialect, toks []token) Decision {
	first := 0
	for first < len(toks) && toks[first].isPunct("(") {
		first++
	}
	if first == len(toks) || toks[first].kind != kindWord {
		return deny("statement does not start with a known keyword")
	}
	rules := pgRules
	if d == MySQL {
		rules = myRules
	}
	check, ok := rules[toks[first].text]
	if !ok {
		return deny("%s statements are not allowed: the proxy is read-only", toks[first].text)
	}

	for i, t := range toks {
		afterDot := i > 0 && toks[i-1].isPunct(".")
		beforeParen := i+1 < len(toks) && toks[i+1].isPunct("(")
		if t.kind == kindWord && !afterDot {
			denied := pgDeniedWord(toks, i)
			if d == MySQL {
				denied = myDeniedWord(toks, i, beforeParen)
			}
			if denied {
				return deny("keyword %s is not allowed: the proxy is read-only", t.text)
			}
		}
		if beforeParen && (t.kind == kindWord || t.kind == kindQuoted) {
			name := strings.ToLower(t.text)
			denied := pgDeniedFunc(name)
			if d == MySQL {
				denied = myDeniedFuncs[name]
			}
			if denied {
				return deny("function %s() is not allowed: the proxy is read-only", name)
			}
		}
	}
	if check == nil {
		return allowed
	}
	return check(toks[first:])
}

func hasWord(toks []token, words ...string) (string, bool) {
	for _, t := range toks {
		if t.kind != kindWord {
			continue
		}
		for _, w := range words {
			if t.text == w {
				return w, true
			}
		}
	}
	return "", false
}

func nextIsWord(toks []token, i int, w string) bool {
	return i+1 < len(toks) && toks[i+1].isWord(w)
}

// noWrite refuses any way of opening a read-write transaction.
func noWrite(toks []token) Decision {
	if _, ok := hasWord(toks, "WRITE"); ok {
		return deny("read-write transactions are not allowed: the proxy is read-only")
	}
	return allowed
}

// noReadOnlySwitch refuses statements that touch the settings holding the
// session in read-only mode (transaction_read_only, default_transaction_read_only,
// tx_read_only, ...), however the name is spelled or quoted.
func noReadOnlySwitch(toks []token) Decision {
	for _, t := range toks {
		switch t.kind {
		case kindWord, kindQuoted, kindString:
			if strings.Contains(strings.ToLower(t.text), "read_only") {
				return deny("changing read-only settings is not allowed")
			}
		}
	}
	return noWrite(toks)
}

func transactionStart(toks []token) Decision {
	if !nextIsWord(toks, 0, "TRANSACTION") {
		return deny("only START TRANSACTION is allowed")
	}
	return noWrite(toks)
}
