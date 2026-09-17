package core

import "strings"

// splitExecutableSubstitutions removes executable expansions from the outer
// command's display form and returns their bodies for independent analysis.
// Substitutions execute inside double quotes but not single quotes. Arithmetic
// is not shell code, but command substitutions inside arithmetic execute too.
// This is a bounded static scanner, not a shell interpreter.
func splitExecutableSubstitutions(raw string, depth int) (string, []string, bool) {
	if depth >= maxCommandNesting {
		return raw, nil, false
	}
	var outer strings.Builder
	var commands []string
	var single, double bool
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == 0 {
			return raw, commands, false
		}
		if single {
			outer.WriteByte(c)
			if c == '\'' {
				single = false
			}
			continue
		}
		if c == '\\' && i+1 < len(raw) {
			outer.WriteString(raw[i : i+2])
			i++
			continue
		}
		if c == '\'' && !double {
			single = true
			outer.WriteByte(c)
			continue
		}
		if c == '"' {
			double = !double
			outer.WriteByte(c)
			continue
		}
		if !double && c == '#' && shellWordStart(raw, i) {
			end := strings.IndexByte(raw[i:], '\n')
			if end < 0 {
				outer.WriteString(raw[i:])
				break
			}
			outer.WriteString(raw[i : i+end])
			i += end - 1
			continue
		}
		if c == '`' {
			body, end, ok := backtickCommand(raw, i)
			if !ok {
				return raw, commands, false
			}
			commands = append(commands, body)
			outer.WriteString("__slb_substitution__")
			i = end
			continue
		}
		if i+1 < len(raw) && raw[i+1] == '(' &&
			(c == '$' || (!double && (c == '<' || c == '>'))) {
			end, ok := shellGroupEnd(raw, i+1, depth+1)
			if !ok {
				return raw, commands, false
			}
			if c == '$' && i+2 < len(raw) && raw[i+2] == '(' {
				if end <= i+3 || raw[end-1] != ')' {
					return raw, commands, false
				}
				_, nested, valid := splitExecutableSubstitutions(raw[i+3:end-1], depth+1)
				commands = append(commands, nested...)
				if !valid {
					return raw, commands, false
				}
				outer.WriteByte('0')
			} else {
				commands = append(commands, raw[i+2:end])
				outer.WriteString("__slb_substitution__")
			}
			i = end
			continue
		}
		outer.WriteByte(c)
	}
	return outer.String(), commands, !single && !double
}

func shellWordStart(raw string, i int) bool {
	return i == 0 || strings.ContainsRune(" \t\r\n;|&(", rune(raw[i-1]))
}

// shellGroupEnd finds the matching ')' with fresh quoting state inside each
// nested substitution. An outer double quote does not quote the code in $(...).
func shellGroupEnd(raw string, open, depth int) (int, bool) {
	if depth >= maxCommandNesting {
		return 0, false
	}
	level := 1
	var single, double bool
	for i := open + 1; i < len(raw); i++ {
		c := raw[i]
		if single {
			if c == '\'' {
				single = false
			}
			continue
		}
		if c == '\\' {
			i++
			continue
		}
		if c == '\'' && !double {
			single = true
			continue
		}
		if c == '"' {
			double = !double
			continue
		}
		if !double && c == '#' && shellWordStart(raw, i) {
			for i+1 < len(raw) && raw[i+1] != '\n' {
				i++
			}
			continue
		}
		if c == '`' {
			_, end, ok := backtickCommand(raw, i)
			if !ok {
				return 0, false
			}
			i = end
			continue
		}
		if i+1 < len(raw) && raw[i+1] == '(' &&
			(c == '$' || (!double && (c == '<' || c == '>'))) {
			end, ok := shellGroupEnd(raw, i+1, depth+1)
			if !ok {
				return 0, false
			}
			i = end
			continue
		}
		if !double {
			switch c {
			case '(':
				level++
			case ')':
				level--
				if level == 0 {
					return i, true
				}
			}
		}
	}
	return 0, false
}

func backtickCommand(raw string, start int) (string, int, bool) {
	var body strings.Builder
	for i := start + 1; i < len(raw); i++ {
		c := raw[i]
		if c == '`' {
			return body.String(), i, true
		}
		if c == '\\' && i+1 < len(raw) && strings.ContainsRune("$`\\\n", rune(raw[i+1])) {
			i++
			if raw[i] != '\n' {
				body.WriteByte(raw[i])
			}
			continue
		}
		body.WriteByte(c)
	}
	return "", 0, false
}
