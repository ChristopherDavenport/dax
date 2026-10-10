// Package cmdline splits a command line into a program and its
// arguments the way a POSIX shell splits words, and does nothing else a
// shell does. dax runs such a command without a shell: the executor's
// launcher (-executor, executor.command), an MCP server (-mcp,
// mcp_servers, /mcp add) and the key command (-api-key-command). One
// splitter for all of them, so a line means the same wherever it is
// given.
package cmdline

import (
	"errors"
	"strings"
)

// Split cuts s into words at unquoted blanks (spaces, tabs, newlines).
// Inside single quotes every character is itself; inside double quotes
// a backslash escapes only ", \, $, ` and a newline (a backslash and
// newline are dropped), and is itself before anything else; outside
// quotes a backslash makes the next character itself, and a backslash
// and newline are dropped. Quoted pieces join the text they touch
// ("/my work"/x is one word), and a pair of quotes with nothing
// between them is an empty word.
//
// Nothing is expanded or interpreted: $VAR, ${VAR}, $(...), `...`, ~,
// globs and the operators (|, ;, &, <, >) are the characters they are,
// since no shell runs the result. An unterminated quote, or a
// backslash at the end, is an error.
func Split(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		in    bool // a word is open, possibly empty ('' opens one)
	)
	end := func() {
		if in {
			words = append(words, cur.String())
			cur.Reset()
			in = false
		}
	}
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			end()
		case c == '\\':
			if i+1 == len(r) {
				return nil, errors.New("a backslash at the end escapes nothing")
			}
			i++
			if r[i] == '\n' {
				continue
			}
			cur.WriteRune(r[i])
			in = true
		case c == '\'':
			j := i + 1
			for j < len(r) && r[j] != '\'' {
				j++
			}
			if j == len(r) {
				return nil, errors.New("a single quote is not closed")
			}
			cur.WriteString(string(r[i+1 : j]))
			in, i = true, j
		case c == '"':
			j := i + 1
			for ; j < len(r) && r[j] != '"'; j++ {
				if r[j] == '\\' && j+1 < len(r) {
					switch r[j+1] {
					case '"', '\\', '$', '`':
						cur.WriteRune(r[j+1])
						j++
						continue
					case '\n':
						j++
						continue
					}
				}
				cur.WriteRune(r[j])
			}
			if j == len(r) {
				return nil, errors.New("a double quote is not closed")
			}
			in, i = true, j
		default:
			cur.WriteRune(c)
			in = true
		}
	}
	end()
	return words, nil
}
