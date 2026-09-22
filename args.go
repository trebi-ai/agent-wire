package agentwire

import "strings"

// SplitArgs splits a prepared command line into arguments.
//
// The rules are the POSIX shell's word splitting, minus expansion: unquoted
// whitespace separates words, single quotes are literal, double quotes keep
// whitespace and honor a backslash before " \ $ ` and newline, and a
// backslash outside quotes escapes the next character. A consumer that pins a
// path with spaces or a literal backslash therefore gets the argument it
// wrote.
func SplitArgs(command string) []string {
	var out []string
	var cur strings.Builder
	started := false
	var quote byte

	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
				continue
			}
			cur.WriteByte(c)
		case quote == '"':
			switch c {
			case '"':
				quote = 0
			case '\\':
				if i+1 < len(command) && isDoubleQuoteEscapable(command[i+1]) {
					i++
					cur.WriteByte(command[i])
					continue
				}
				cur.WriteByte(c)
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			started = true
		case c == '\\':
			if i+1 < len(command) {
				i++
				cur.WriteByte(command[i])
				started = true
				continue
			}
			cur.WriteByte(c)
		case c == ' ' || c == '\t' || c == '\n':
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return out
}

// isDoubleQuoteEscapable reports the characters a backslash escapes inside
// double quotes, per POSIX.
func isDoubleQuoteEscapable(c byte) bool {
	switch c {
	case '"', '\\', '$', '`', '\n':
		return true
	}
	return false
}
