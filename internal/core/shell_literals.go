package core

import (
	"path/filepath"
	"regexp"
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
//   - the script contains no construct that can assign a variable whose name
//     is computed at run time (dynamic declare/read/printf -v/..., namerefs,
//     indirect expansion assignments) and never mentions IFS.
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
	if file == nil || identifierCount(raw, "IFS") > 0 {
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
			if !ok || !literalVariableValue.MatchString(value) || variableAssigningBuiltins[filepath.Base(value)] {
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
			assigning, computed := false, false
			for _, arg := range node.Args {
				if lit := arg.Lit(); lit != "" {
					assigning = assigning || variableAssigningBuiltins[lit]
				} else {
					computed = true
				}
			}
			dynamicAssignment = dynamicAssignment || (assigning && computed)
		}
		return !dynamicAssignment
	})
	if dynamicAssignment {
		return nil
	}

	vars := literalVariables{}
	for name, assigned := range candidates {
		if len(assigned) != 1 {
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
