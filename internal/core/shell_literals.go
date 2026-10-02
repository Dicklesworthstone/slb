package core

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Static resolution of script-local literal variables for the control-flow
// (AST) classifier (GitHub #20).
//
// A computed executable such as `$S daemon status` cannot be classified, so
// the AST path treats it as a parse error. Agents often write
// `S=/path/to/tool; if $S status | grep -q x; then ...; fi`, where the value
// is a literal assigned a few characters earlier in the same script. Resolving
// that reference makes the command visible to risk patterns instead of
// opaque: `X=rm; if true; then $X -rf /; fi` becomes `rm -rf /`.
//
// Resolution is deliberately narrow and must never guess. A variable resolves
// only when all of the following hold; otherwise the reference stays computed
// and fails closed exactly as before:
//   - it is assigned exactly once, by a plain top-level assignment statement
//     (`NAME=value`, not a prefix assignment, pipeline element, background job,
//     loop body, branch or function), before the reference;
//   - the value is a literal made of path-safe characters (no whitespace,
//     globs, quotes, tilde or expansions), so word splitting and pathname
//     expansion cannot change it;
//   - every other occurrence of the name in the raw script is a plain
//     `$NAME` / `${NAME}` read, so no read/for/printf -v/declare/export,
//     `${NAME:=x}`, comment or string mentions it;
//   - no word passed to an assigning builtin contains the name after quote
//     removal (`printf -v X\Y`, `read -aXY`, `unset X\Y` name XY without
//     spelling it as an identifier);
//   - the script contains no construct that can assign a variable whose name
//     is computed at run time (dynamic declare/read/printf -v/..., namerefs,
//     indirect expansion assignments), no trap/enable, and never names IFS.
type literalVariable struct {
	value string
	after uint // byte offset from which the assignment is in effect
}

type literalVariables map[string]literalVariable

var literalVariableValue = regexp.MustCompile(`^[A-Za-z0-9_./+:@%,-]+$`)

// Builtins that can assign a variable named by one of their arguments.
var variableAssigningBuiltins = map[string]bool{
	"read": true, "mapfile": true, "readarray": true, "printf": true, "getopts": true,
	"wait": true, "eval": true, "source": true, ".": true, "let": true, "unset": true,
	"export": true, "declare": true, "typeset": true, "local": true, "readonly": true,
}

// Builtins that run code the scan cannot see in the current shell: a trap
// action is evaluated like eval (e.g. on every command with DEBUG), enable -f
// loads a builtin from a shared object, and an alias body replaces a later
// command word (`alias printf='printf -vXY'`). Their mere presence disables
// resolution.
var codeLoadingBuiltins = map[string]bool{"trap": true, "enable": true, "alias": true}

// resolve returns the literal value of a plain parameter expansion, if the
// expansion refers to a resolvable variable read after its assignment.
func (vars literalVariables) resolve(param *syntax.ParamExp) (string, bool) {
	if len(vars) == 0 || param == nil || param.Param == nil || !plainParamExp(param) {
		return "", false
	}
	variable, ok := vars[param.Param.Value]
	if !ok || param.Pos().Offset() < variable.after {
		return "", false
	}
	return variable.value, true
}

func plainParamExp(param *syntax.ParamExp) bool {
	return !param.Excl && !param.Length && !param.Width && param.Index == nil &&
		param.Slice == nil && param.Repl == nil && param.Names == 0 && param.Exp == nil
}

func collectLiteralVariables(file *syntax.File, raw string) literalVariables {
	if file == nil || identifierCount(raw, "IFS") > 0 || identifierCount(raw, "BASH_ALIASES") > 0 {
		return nil
	}

	type candidate struct {
		value string
		after uint
	}
	candidates := map[string][]candidate{}
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) != 0 || len(stmt.Redirs) != 0 || stmt.Negated || stmt.Background || stmt.Coprocess {
			continue
		}
		for _, assign := range call.Assigns {
			if assign.Name == nil || assign.Append || assign.Naked || assign.Index != nil ||
				assign.Array != nil || assign.Value == nil {
				continue
			}
			value, ok := literalWordValue(assign.Value)
			// A value naming a variable-assigning builtin would hide a
			// dynamic assignment from the scan below; leave it computed.
			base := filepath.Base(value)
			if !ok || !literalVariableValue.MatchString(value) || variableAssigningBuiltins[base] || codeLoadingBuiltins[base] {
				continue
			}
			candidates[assign.Name.Value] = append(candidates[assign.Name.Value],
				candidate{value: value, after: stmt.End().Offset()})
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	dynamicAssignment := false
	plainReads := map[string]int{}
	var assignerWords []string // static words of calls to assigning builtins
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node := node.(type) {
		case *syntax.ParamExp:
			if node.Param == nil {
				break
			}
			if node.Exp != nil && node.Excl &&
				(node.Exp.Op == syntax.AssignUnset || node.Exp.Op == syntax.AssignUnsetOrNull) {
				dynamicAssignment = true
			}
			if plainParamExp(node) {
				plainReads[node.Param.Value]++
			}
		case *syntax.DeclClause:
			for _, assign := range node.Args {
				if assign.Name != nil {
					continue
				}
				// Options are naked literal words; anything else names a
				// variable at run time. -n/+n create namerefs.
				flag := ""
				if assign.Value != nil {
					flag = assign.Value.Lit()
				}
				if !strings.HasPrefix(flag, "-") || strings.Contains(flag, "n") {
					dynamicAssignment = true
				}
			}
		case *syntax.CallExpr:
			// Judge words by the value bash sees after quote removal, not by
			// their source spelling: `printf -v X\Y v`, `pr""intf -vXY v`,
			// `read X{Y,Z}` and `unset X\Y` all name XY although XY never
			// appears as an identifier in the raw text.
			assigning, computed := false, false
			var values []string
			// Words from this index on are printf data operands, which can
			// never name a variable (GitHub #23); -1 when not proven.
			dataStart := printfDataStart(node.Args)
			for i, arg := range node.Args {
				value, ok := staticWordValue(arg)
				if !ok {
					if dataStart < 0 || i < dataStart {
						computed = true
					}
					continue
				}
				assigning = assigning || variableAssigningBuiltins[value]
				dynamicAssignment = dynamicAssignment || codeLoadingBuiltins[value]
				values = append(values, value)
			}
			// mapfile/readarray -C evaluates its callback text like eval.
			callback := false
			for _, value := range values {
				callback = callback || (strings.HasPrefix(value, "-") && strings.Contains(value, "C"))
			}
			if callback && (slices.Contains(values, "mapfile") || slices.Contains(values, "readarray")) {
				dynamicAssignment = true
			}
			if assigning {
				if computed {
					dynamicAssignment = true
				} else {
					assignerWords = append(assignerWords, values...)
				}
			}
		}
		return !dynamicAssignment
	})
	if dynamicAssignment {
		return nil
	}
	// A name can be passed to an assigning builtin inside a longer word
	// (`printf -vIFS x`, `read -aXY`), so any substring match counts.
	assignedBy := func(name string) bool {
		for _, word := range assignerWords {
			if strings.Contains(word, name) {
				return true
			}
		}
		return false
	}
	if assignedBy("IFS") {
		return nil
	}

	vars := literalVariables{}
	for name, assigned := range candidates {
		if len(assigned) != 1 || assignedBy(name) {
			continue
		}
		// Every mention of the name must be the assignment or a plain read.
		if identifierCount(raw, name) != 1+plainReads[name] {
			continue
		}
		vars[name] = literalVariable{value: assigned[0].value, after: assigned[0].after}
	}
	return vars
}

// printfDataStart returns the index of the first data operand of a call to
// the printf builtin whose option and format words are all static, or -1.
//
// Bash's printf accepts options (only -v NAME assigns) before its format
// operand, and option parsing ends at the first word that does not start
// with "-" (a lone "-" is a format) or after "--". Every later word is data
// for the format: `printf '%s' -v S` prints "-v" and "S", and an expansion
// there cannot become an option whatever its value. An expansion before the
// format could (`x=-vS; printf "$x" v`), so such calls are not proven.
//
// The call must be printf itself, with no other word naming an assigning or
// code-loading builtin, so a wrapper (`builtin printf`) or a data word that
// a redefined printf might run stays fail-closed. Functions are walked like
// any other code, and alias definitions disable resolution altogether.
func printfDataStart(args []*syntax.Word) int {
	if len(args) < 2 {
		return -1
	}
	if name, ok := staticWordValue(args[0]); !ok || name != "printf" {
		return -1
	}
	format := -1
	for i := 1; i < len(args) && format < 0; i++ {
		value, ok := staticWordValue(args[i])
		switch {
		case !ok:
			return -1 // could expand to an option, or to nothing
		case value == "--":
			if i+1 < len(args) {
				format = i + 1
			} else {
				return -1
			}
		case value == "-v":
			i++ // the variable name, judged as an assigner word
		case strings.HasPrefix(value, "-") && value != "-":
			// -vNAME or an invalid option (printf then fails).
		default:
			format = i
		}
	}
	if format < 0 {
		return -1
	}
	if value, ok := staticWordValue(args[format]); !ok || !printfTextConversions(value) {
		return -1
	}
	for i, arg := range args {
		value, ok := staticWordValue(arg)
		if i > 0 && ok && (variableAssigningBuiltins[value] || codeLoadingBuiltins[value]) {
			return -1
		}
	}
	return format + 1
}

// printfTextConversions reports a printf format whose conversions all take
// their argument as text (%s, %b, %q, %c) or none (%%), with no * width or
// precision. zsh evaluates the argument of a numeric conversion, and of a *
// width, as an arithmetic expression, which can assign a variable
// (`printf '%d' "$x"` with x='S=1' sets S).
func printfTextConversions(format string) bool {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("-+ #0123456789.$'", format[j]) >= 0 {
			j++
		}
		if j >= len(format) || strings.IndexByte("sbqc%", format[j]) < 0 {
			return false
		}
		i = j
	}
	return true
}

// literalWordValue returns the value of a word made only of literal,
// single-quoted and double-quoted-literal parts.
func literalWordValue(word *syntax.Word) (string, bool) {
	var out strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			if strings.ContainsRune(part.Value, '\\') {
				return "", false
			}
			out.WriteString(part.Value)
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", false
			}
			out.WriteString(part.Value)
		case *syntax.DblQuoted:
			if part.Dollar {
				return "", false
			}
			for _, inner := range part.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok || strings.ContainsRune(lit.Value, '\\') {
					return "", false
				}
				out.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return out.String(), out.Len() > 0
}

// staticWordValue returns the value bash gives word after quote removal, when
// that value cannot depend on expansion: no parameter, command, arithmetic or
// ANSI-C/locale quoting, and no unquoted brace, glob or tilde characters
// (rejected even when escaped, to stay conservative).
func staticWordValue(word *syntax.Word) (string, bool) {
	var out strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(part.Value, "{}*?[~") {
				return "", false
			}
			value := part.Value
			for i := 0; i < len(value); i++ {
				if value[i] != '\\' || i+1 == len(value) {
					out.WriteByte(value[i])
					continue
				}
				i++
				if value[i] != '\n' { // backslash-newline is a line continuation
					out.WriteByte(value[i])
				}
			}
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", false
			}
			out.WriteString(part.Value)
		case *syntax.DblQuoted:
			if part.Dollar {
				return "", false
			}
			for _, inner := range part.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				value := lit.Value
				for i := 0; i < len(value); i++ {
					if value[i] == '\\' && i+1 < len(value) && strings.IndexByte("$`\"\\\n", value[i+1]) >= 0 {
						i++
						if value[i] != '\n' {
							out.WriteByte(value[i])
						}
						continue
					}
					out.WriteByte(value[i])
				}
			}
		default:
			return "", false
		}
	}
	return out.String(), true
}

// identifierCount counts maximal [A-Za-z0-9_] runs in raw equal to name.
func identifierCount(raw, name string) int {
	count := 0
	for i := 0; i < len(raw); {
		if !isIdentifierByte(raw[i]) {
			i++
			continue
		}
		j := i
		for j < len(raw) && isIdentifierByte(raw[j]) {
			j++
		}
		if raw[i:j] == name {
			count++
		}
		i = j
	}
	return count
}

func isIdentifierByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
