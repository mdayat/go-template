package main

import (
	"slices"
	"strings"
)

// tokenKind is what a token is, as far as the checks care.
type tokenKind int

const (
	// keyword is a keyword or an unquoted name, in capitals.
	keyword tokenKind = iota
	// quotedName is a "quoted name", without its quotes.
	quotedName
	// literal is text, a number or a function body: never a keyword, whatever it holds.
	literal
	// symbol is one character of anything else: ( ) , . ;
	symbol
)

type token struct {
	kind tokenKind
	text string
}

// is says whether t is one of the keywords.
func (t token) is(keywords ...string) bool {
	return t.kind == keyword && slices.Contains(keywords, t.text)
}

// statement is one SQL statement.
type statement struct {
	line   int
	tokens []token
	// comments holds the -- comments on lines of their own above the statement (after the one
	// before it), without the -- and the spaces around.
	comments []string
}

// parse splits src into statements, leaving out comments and the contents of text, so a keyword
// in either is never taken for SQL.
func parse(src string) []statement {
	var stmts []statement
	var cur statement
	start := 0 // where cur's first token is
	add := func(kind tokenKind, text string, at int) {
		if len(cur.tokens) == 0 {
			start = at
		}
		cur.tokens = append(cur.tokens, token{kind, text})
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case strings.HasPrefix(src[i:], "--"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			lineStart := strings.LastIndexByte(src[:i], '\n') + 1
			if len(cur.tokens) == 0 && strings.TrimSpace(src[lineStart:i]) == "" {
				cur.comments = append(cur.comments, strings.TrimSpace(src[i+2:i+end]))
			}
			i += end
		case strings.HasPrefix(src[i:], "/*"):
			i = blockCommentEnd(src, i)
		case c == '\'':
			end := quotedEnd(src, i, '\'', false)
			add(literal, src[i:end], i)
			i = end
		case c == '"':
			end := quotedEnd(src, i, '"', false)
			add(quotedName, strings.ReplaceAll(strings.Trim(src[i:end], `"`), `""`, `"`), i)
			i = end
		case c == '$':
			end := dollarQuotedEnd(src, i)
			if end < 0 {
				add(symbol, "$", i)
				i++
				break
			}
			add(literal, src[i:end], i)
			i = end
		case isWordStart(c):
			end := i + 1
			for end < len(src) && (isWordStart(src[end]) || isDigit(src[end]) || src[end] == '$') {
				end++
			}
			word := src[i:end]
			if strings.EqualFold(word, "E") && end < len(src) && src[end] == '\'' {
				// E'...': text in which a backslash escapes the next character.
				strEnd := quotedEnd(src, end, '\'', true)
				add(literal, src[i:strEnd], i)
				i = strEnd
				break
			}
			add(keyword, strings.ToUpper(word), i)
			i = end
		case isDigit(c):
			end := i + 1
			for end < len(src) && (isDigit(src[end]) || src[end] == '.') {
				end++
			}
			add(literal, src[i:end], i)
			i = end
		case c == ';':
			if len(cur.tokens) > 0 {
				cur.line = 1 + strings.Count(src[:start], "\n")
				stmts = append(stmts, cur)
			}
			cur = statement{}
			i++
		default:
			add(symbol, string(c), i)
			i++
		}
	}
	if len(cur.tokens) > 0 {
		cur.line = 1 + strings.Count(src[:start], "\n")
		stmts = append(stmts, cur)
	}
	return stmts
}

func isWordStart(c byte) bool {
	return c == '_' || c >= 0x80 || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// blockCommentEnd returns the index just past the /* comment */ at i; they nest in Postgres.
func blockCommentEnd(src string, i int) int {
	depth := 0
	for i < len(src) {
		switch {
		case strings.HasPrefix(src[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(src[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(src)
}

// quotedEnd returns the index just past the quoted text at i (src[i] is q). A doubled q stands
// for itself; with backslash, a backslash escapes the next character too.
func quotedEnd(src string, i int, q byte, backslash bool) int {
	for j := i + 1; j < len(src); j++ {
		switch {
		case backslash && src[j] == '\\':
			j++
		case src[j] == q:
			if j+1 < len(src) && src[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(src)
}

// dollarQuotedEnd returns the index just past the $tag$...$tag$ text at i, or -1 if no tag starts
// there (a $1 parameter, say).
func dollarQuotedEnd(src string, i int) int {
	j := i + 1
	for j < len(src) && (isWordStart(src[j]) || (j > i+1 && isDigit(src[j]))) {
		j++
	}
	if j >= len(src) || src[j] != '$' {
		return -1
	}
	tag := src[i : j+1]
	end := strings.Index(src[j+1:], tag)
	if end < 0 {
		return len(src)
	}
	return j + 1 + end + len(tag)
}

// startsWith says whether ts starts with the keywords, in order.
func startsWith(ts []token, keywords ...string) bool {
	if len(ts) < len(keywords) {
		return false
	}
	for i, k := range keywords {
		if !ts[i].is(k) {
			return false
		}
	}
	return true
}

// skip drops the keywords from the start of ts if it starts with them all.
func skip(ts []token, keywords ...string) []token {
	if startsWith(ts, keywords...) {
		return ts[len(keywords):]
	}
	return ts
}

// hasWord says whether ts holds the keyword.
func hasWord(ts []token, kw string) bool {
	return slices.ContainsFunc(ts, func(t token) bool { return t.is(kw) })
}

// hasSeq says whether ts holds the keywords one after another.
func hasSeq(ts []token, keywords ...string) bool {
	for i := range ts {
		if startsWith(ts[i:], keywords...) {
			return true
		}
	}
	return false
}

// flat returns ts without whatever is inside parentheses: what a clause says itself, not an
// expression or a list in it (CHECK (x IS NOT NULL) isn't a NOT NULL column).
func flat(ts []token) []token {
	var out []token
	depth := 0
	for _, t := range ts {
		switch {
		case t.kind == symbol && t.text == "(":
			depth++
		case t.kind == symbol && t.text == ")":
			depth--
		case depth == 0:
			out = append(out, t)
		}
	}
	return out
}

// splitTop splits ts at the commas outside parentheses: an ALTER TABLE's actions.
func splitTop(ts []token) [][]token {
	var parts [][]token
	depth, from := 0, 0
	for i, t := range ts {
		switch {
		case t.kind == symbol && t.text == "(":
			depth++
		case t.kind == symbol && t.text == ")":
			depth--
		case t.kind == symbol && t.text == "," && depth == 0:
			parts = append(parts, ts[from:i])
			from = i + 1
		}
	}
	return append(parts, ts[from:])
}

// name reads a possibly schema-qualified name at the start of ts, returning its last part as
// Postgres stores it (unquoted names in lower case) and the tokens after it.
func name(ts []token) (string, []token) {
	var last string
	for len(ts) > 0 {
		switch ts[0].kind {
		case keyword:
			last = strings.ToLower(ts[0].text)
		case quotedName:
			last = ts[0].text
		default:
			return last, ts
		}
		ts = ts[1:]
		if len(ts) == 0 || ts[0].kind != symbol || ts[0].text != "." {
			return last, ts
		}
		ts = ts[1:]
	}
	return last, ts
}

// names reads a comma-separated list of names (TRUNCATE a, b; DROP TABLE a, b).
func names(ts []token) []string {
	var out []string
	for {
		n, rest := name(ts)
		if n == "" {
			return out
		}
		out = append(out, n)
		if len(rest) == 0 || rest[0].kind != symbol || rest[0].text != "," {
			return out
		}
		ts = rest[1:]
	}
}
