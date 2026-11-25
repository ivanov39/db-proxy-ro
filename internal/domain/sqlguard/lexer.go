package sqlguard

import (
	"errors"
	"strings"
)

// Dialect selects the lexical and grammatical rules used to inspect a query.
type Dialect int

const (
	Postgres Dialect = iota
	MySQL
)

type tokenKind uint8

const (
	kindWord   tokenKind = iota // bare identifier or keyword; text is upper-cased
	kindQuoted                  // quoted identifier ("x", `x`) or a MySQL "string"; text is the content
	kindString                  // string literal; text is the raw content
	kindNumber
	kindParam // $1
	kindPunct // any other single byte
)

type token struct {
	kind tokenKind
	text string
}

func (t token) isPunct(s string) bool { return t.kind == kindPunct && t.text == s }
func (t token) isWord(s string) bool  { return t.kind == kindWord && t.text == s }

// lexMode lists the server settings that change how a query is split into tokens.
// The guard cannot know their current values, so it lexes a query under every
// combination and accepts it only if all of them are acceptable.
type lexMode struct {
	// backslashSingle: a backslash escapes the next character inside '...'.
	// PostgreSQL: standard_conforming_strings=off. MySQL: NO_BACKSLASH_ESCAPES is not set.
	backslashSingle bool
	// backslashDouble: the same for MySQL "..." literals (they are identifiers
	// without escapes when ANSI_QUOTES is set).
	backslashDouble bool
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }

// lex splits a query into tokens, dropping whitespace and comments.
//
// The lexer is deliberately conservative: whenever a construct could be read
// differently by the real server (and so could hide code from the guard), it
// either returns an error or picks the reading that exposes more code.
func lex(sql string, d Dialect, m lexMode) ([]token, error) {
	var toks []token
	n := len(sql)
	i := 0
	for i < n {
		c := sql[i]
		switch {
		case isSpace(c):
			i++

		case c == '-' && i+1 < n && sql[i+1] == '-':
			// MySQL needs whitespace after "--", and what counts as whitespace
			// depends on the connection charset. Refuse anything unclear.
			if d == MySQL && i+2 < n && !isSpace(sql[i+2]) {
				return nil, errors.New(`ambiguous "--": must be followed by whitespace`)
			}
			i = skipLineComment(sql, i)

		case c == '#' && d == MySQL:
			i = skipLineComment(sql, i)

		case c == '/' && i+1 < n && sql[i+1] == '*':
			end, err := skipBlockComment(sql, i, d)
			if err != nil {
				return nil, err
			}
			i = end

		case c == '\'':
			end, err := scanQuoted(sql, i, '\'', m.backslashSingle)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kindString, sql[i+1 : end-1]})
			i = end

		case c == '"':
			// PostgreSQL: always an identifier, no backslash escapes.
			end, err := scanQuoted(sql, i, '"', d == MySQL && m.backslashDouble)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kindQuoted, strings.ReplaceAll(sql[i+1:end-1], `""`, `"`)})
			i = end

		case c == '`' && d == MySQL:
			end, err := scanQuoted(sql, i, '`', false)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kindQuoted, strings.ReplaceAll(sql[i+1:end-1], "``", "`")})
			i = end

		case c == '$' && d == Postgres:
			if i+1 < n && isDigit(sql[i+1]) {
				j := i + 1
				for j < n && isDigit(sql[j]) {
					j++
				}
				toks = append(toks, token{kindParam, sql[i:j]})
				i = j
				break
			}
			if tagEnd, ok := dollarTag(sql, i); ok {
				tag := sql[i:tagEnd]
				closeAt := strings.Index(sql[tagEnd:], tag)
				if closeAt < 0 {
					return nil, errors.New("unterminated dollar-quoted string")
				}
				toks = append(toks, token{kindString, sql[tagEnd : tagEnd+closeAt]})
				i = tagEnd + closeAt + len(tag)
				break
			}
			toks = append(toks, token{kindPunct, "$"})
			i++

		case isDigit(c) || (c == '.' && i+1 < n && isDigit(sql[i+1])):
			j := i + 1
			for j < n && (isDigit(sql[j]) || sql[j] == '.' || isIdentStart(sql[j]) || (d == MySQL && sql[j] == '$')) {
				j++
			}
			toks = append(toks, token{kindNumber, sql[i:j]})
			i = j

		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentPart(sql[j]) {
				j++
			}
			word := sql[i:j]
			if d == Postgres && len(word) == 1 && j < n {
				switch {
				case (word == "E" || word == "e") && sql[j] == '\'':
					// Escape string: backslashes always escape.
					end, err := scanQuoted(sql, j, '\'', true)
					if err != nil {
						return nil, err
					}
					toks = append(toks, token{kindString, sql[j+1 : end-1]})
					i = end
					continue
				case (word == "U" || word == "u") && sql[j] == '&' && j+1 < n && sql[j+1] == '"':
					return nil, errors.New(`Unicode-escaped identifiers (U&"...") are not allowed`)
				}
			}
			toks = append(toks, token{kindWord, strings.ToUpper(word)})
			i = j

		default:
			toks = append(toks, token{kindPunct, sql[i : i+1]})
			i++
		}
	}
	return toks, nil
}

// skipLineComment returns the index just past a "--" or "#" comment. The comment
// is ended at the earliest character any server might treat as its end, so text
// after it is inspected as code rather than hidden.
func skipLineComment(sql string, i int) int {
	for i < len(sql) && sql[i] != '\n' && sql[i] != '\r' && sql[i] != 0 {
		i++
	}
	return i
}

func skipBlockComment(sql string, i int, d Dialect) (int, error) {
	if d == MySQL {
		// Executable comments are refused before lexing (see Check).
		end := strings.Index(sql[i+2:], "*/")
		if end < 0 {
			return 0, errors.New("unterminated comment")
		}
		return i + 2 + end + 2, nil
	}
	// PostgreSQL comments nest.
	depth := 1
	j := i + 2
	for j < len(sql) && depth > 0 {
		switch {
		case strings.HasPrefix(sql[j:], "/*"):
			depth++
			j += 2
		case strings.HasPrefix(sql[j:], "*/"):
			depth--
			j += 2
		default:
			j++
		}
	}
	if depth > 0 {
		return 0, errors.New("unterminated comment")
	}
	return j, nil
}

// scanQuoted returns the index just past the closing quote of the literal that
// starts at sql[i]. A doubled quote is always an escaped quote.
func scanQuoted(sql string, i int, quote byte, backslash bool) (int, error) {
	for j := i + 1; j < len(sql); j++ {
		switch {
		case backslash && sql[j] == '\\':
			j++ // skip the escaped character
		case sql[j] == quote:
			if j+1 < len(sql) && sql[j+1] == quote {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return 0, errors.New("unterminated quoted literal")
}

// dollarTag reports whether a PostgreSQL dollar-quote opener ($$ or $tag$)
// starts at sql[i] and returns the index just past it.
func dollarTag(sql string, i int) (int, bool) {
	j := i + 1
	for j < len(sql) {
		c := sql[j]
		if c == '$' {
			return j + 1, true
		}
		first := j == i+1
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80 || (!first && isDigit(c)) {
			j++
			continue
		}
		return 0, false
	}
	return 0, false
}
