// Package proxycmd recognizes the MySQL statements the proxy answers itself
// instead of sending them to a database.
package proxycmd

import (
	"regexp"
	"strings"
)

type Kind int

const (
	// None: an ordinary statement, not a proxy command.
	None Kind = iota
	// ShowDatabases: SHOW DATABASES [LIKE 'pattern'] — list the databases
	// configured in the proxy.
	ShowDatabases
	// Unsupported: a form of SHOW DATABASES the proxy cannot answer, such as
	// SHOW DATABASES WHERE ...
	Unsupported
	// Use: USE name — select a database (MySQL).
	Use
)

// Command is a parsed proxy command.
type Command struct {
	Kind       Kind
	Pattern    string // LIKE pattern of ShowDatabases
	HasPattern bool
	Name       string // database of Use
}

// UnsupportedReason explains an Unsupported command to the client.
const UnsupportedReason = "only SHOW DATABASES and SHOW DATABASES LIKE 'pattern' are supported, as plain (not prepared) queries"

var (
	showRe    = regexp.MustCompile(`(?is)^\s*show\s+(?:databases|schemas)\s*(?:like\s+'((?:[^']|'')*)'\s*)?;?\s*$`)
	showAnyRe = regexp.MustCompile(`(?is)^\s*show\s+(?:databases|schemas)\b`)
	useRe     = regexp.MustCompile("(?is)^\\s*use\\s+(?:`((?:[^`]|``)+)`|([^\\s;`]+))\\s*;?\\s*$")
)

// Parse classifies a SQL text.
func Parse(sql string) Command {
	if m := showRe.FindStringSubmatchIndex(sql); m != nil {
		if m[2] < 0 {
			return Command{Kind: ShowDatabases}
		}
		pattern := strings.ReplaceAll(sql[m[2]:m[3]], "''", "'")
		return Command{Kind: ShowDatabases, Pattern: pattern, HasPattern: true}
	}
	if showAnyRe.MatchString(sql) {
		return Command{Kind: Unsupported}
	}
	if m := useRe.FindStringSubmatch(sql); m != nil {
		if m[1] != "" {
			return Command{Kind: Use, Name: strings.ReplaceAll(m[1], "``", "`")}
		}
		return Command{Kind: Use, Name: m[2]}
	}
	return Command{}
}

// Filter returns the names the command selects.
func (c Command) Filter(names []string) []string {
	if !c.HasPattern {
		return names
	}
	re := likeToRegexp(c.Pattern)
	var out []string
	for _, n := range names {
		if re.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

// likeToRegexp translates a SQL LIKE pattern: % is any text, _ is any one
// character, a backslash makes the next character literal. Matching is
// case-insensitive, as in MySQL.
func likeToRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?is)^")
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; {
		case r == '%':
			b.WriteString(".*")
		case r == '_':
			b.WriteString(".")
		case r == '\\' && i+1 < len(runes):
			i++
			b.WriteString(regexp.QuoteMeta(string(runes[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
