package sqlguard

var myRules = map[string]rule{
	"SELECT": nil, "WITH": nil, "TABLE": nil, "VALUES": nil,
	"SHOW": nil, "DESCRIBE": nil, "DESC": nil, "EXPLAIN": nil,
	"USE": nil, "HELP": nil, "CHECKSUM": nil,
	"COMMIT": nil, "ROLLBACK": nil, "SAVEPOINT": nil, "RELEASE": nil,

	"BEGIN": myBegin,
	"START": transactionStart,
	"SET":   mySet,
}

// myDeniedWord reports keywords that turn an otherwise read-only statement
// into a writing or locking one: WITH ... UPDATE/DELETE, SELECT ... INTO
// OUTFILE/DUMPFILE/@var, locking reads, MariaDB sequences.
func myDeniedWord(toks []token, i int, beforeParen bool) bool {
	switch toks[i].text {
	case "UPDATE", "DELETE", "INTO":
		return true
	case "INSERT", "REPLACE":
		return !beforeParen // INSERT() and REPLACE() are string functions
	case "SHARE":
		return i > 0 && toks[i-1].isWord("FOR")
	case "LOCK":
		return nextIsWord(toks, i, "IN")
	case "NEXT":
		return nextIsWord(toks, i, "VALUE")
	}
	return false
}

var myDeniedFuncs = map[string]bool{
	"get_lock": true, "release_lock": true, "release_all_locks": true,
	"nextval": true, "setval": true,
	"sys_exec": true, "sys_eval": true,
}

func myBegin(toks []token) Decision {
	if len(toks) == 1 || (len(toks) == 2 && toks[1].isWord("WORK")) {
		return allowed
	}
	return deny("only BEGIN [WORK] is allowed")
}

// mySet allows session-level SET only.
func mySet(toks []token) Decision {
	if w, ok := hasWord(toks, "GLOBAL", "PERSIST", "PERSIST_ONLY", "PASSWORD", "ROLE", "RESOURCE"); ok {
		return deny("SET %s is not allowed: the proxy is read-only", w)
	}
	return noReadOnlySwitch(toks)
}
