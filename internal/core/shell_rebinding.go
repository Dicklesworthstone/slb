package core

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Assignments that change what later code runs, which the feed analysis
// would otherwise miss because they hide behind quoting or expansion:
//
//   - BASH_ALIASES / BASH_CMDS written through a name operand whose spelling
//     is split by quotes or escapes (`printf -v BASH_\CMDS[ls] /bin/rm`,
//     `declare "BASH_C""MDS[ls]=..."`): the array rebinds a command name;
//   - a name operand computed at run time (`printf -v "$n"`, `read "$n"`,
//     `declare "$n=..."`, a nameref to a computed name): it can be either
//     array;
//   - an array subscript in a name operand, which bash evaluates as
//     arithmetic, expanding `$(...)` inside it even when the operand was
//     single-quoted (`read 'a[$(cmd)]'` runs cmd).
//
// Each makes the command opaque (at least CAUTION); a static command
// substitution inside a subscript is also classified as code.

// rebindingMarker stands for a non-literal part of a word.
const rebindingMarker = "\x00"

// markedWordText returns word after quote removal with every expansion
// replaced by rebindingMarker, except provably literal variables, which are
// replaced by their values. Brackets and globs are kept verbatim.
func (a *feedAnalysis) markedWordText(word *syntax.Word) string {
	if word == nil {
		return ""
	}
	var out strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			out.WriteString(unescapeUnquoted(part.Value))
		case *syntax.SglQuoted:
			if part.Dollar {
				out.WriteString(rebindingMarker)
				continue
			}
			out.WriteString(part.Value)
		case *syntax.DblQuoted:
			if part.Dollar {
				out.WriteString(rebindingMarker)
				continue
			}
			for _, inner := range part.Parts {
				switch inner := inner.(type) {
				case *syntax.Lit:
					out.WriteString(unescapeDoubleQuoted(inner.Value))
				case *syntax.ParamExp:
					out.WriteString(a.literalOrMarker(inner))
				default:
					out.WriteString(rebindingMarker)
				}
			}
		case *syntax.ParamExp:
			out.WriteString(a.literalOrMarker(part))
		default:
			out.WriteString(rebindingMarker)
		}
	}
	return out.String()
}

func (a *feedAnalysis) literalOrMarker(param *syntax.ParamExp) string {
	if value, ok := a.literals.resolve(param); ok {
		return value
	}
	return rebindingMarker
}

func rebindingArray(text string) bool {
	return strings.Contains(text, "BASH_ALIASES") || strings.Contains(text, "BASH_CMDS")
}

// declBuiltins assign the NAME[=VALUE] operands they are given.
var declBuiltins = map[string]bool{
	"declare": true, "typeset": true, "local": true, "export": true, "readonly": true, "nameref": true,
}

// checkRebinding inspects every name operand of an assigning builtin in file.
func (a *feedAnalysis) checkRebinding(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node := node.(type) {
		case *syntax.Word:
			if rebindingArray(a.markedWordText(node)) {
				a.opaque = true
			}
		case *syntax.DeclClause:
			nameref := node.Variant != nil && node.Variant.Value == "nameref"
			var operands []string
			for _, assign := range node.Args {
				switch {
				case assign.Name != nil:
					operand := assign.Name.Value
					if assign.Value != nil {
						operand += "=" + a.markedWordText(assign.Value)
					}
					operands = append(operands, operand)
				case assign.Value != nil:
					text := a.markedWordText(assign.Value)
					if (strings.HasPrefix(text, "-") || strings.HasPrefix(text, "+")) && !strings.Contains(text, "=") {
						nameref = nameref || strings.Contains(text, "n")
						if strings.Contains(text, rebindingMarker) {
							a.opaque = true // a computed option
						}
						continue
					}
					operands = append(operands, text)
				}
			}
			for _, operand := range operands {
				name, value, _ := strings.Cut(operand, "=")
				a.checkNameOperand(name)
				if nameref && strings.Contains(value, rebindingMarker) {
					a.opaque = true // a nameref to a computed name
				}
			}
		case *syntax.CallExpr:
			tokens := make([]string, len(node.Args))
			for i, word := range node.Args {
				tokens[i] = a.markedWordText(word)
			}
			names, subscriptsOnly := assignedNameOperands(tokens)
			for _, name := range names {
				if subscriptsOnly {
					a.checkSubscript(name)
				} else {
					a.checkNameOperand(name)
				}
			}
		}
		return true
	})
}

// checkNameOperand judges one variable name passed to an assigning builtin.
func (a *feedAnalysis) checkNameOperand(name string) {
	base, subscript, hasSubscript := strings.Cut(name, "[")
	if strings.Contains(base, rebindingMarker) || rebindingArray(base) {
		a.opaque = true
		return
	}
	if hasSubscript {
		a.checkSubscript("[" + subscript)
	}
}

// checkSubscript judges the array subscripts in an operand that bash
// evaluates as arithmetic. An expansion there that holds $(...) runs it, and
// so does a literal $(...) (`read 'a[$(cmd)]'`).
func (a *feedAnalysis) checkSubscript(operand string) {
	_, subscript, ok := strings.Cut(operand, "[")
	if !ok {
		return
	}
	if strings.Contains(subscript, rebindingMarker) {
		// An expanded subscript is expanded again as arithmetic, so a value
		// holding $(...) runs it.
		a.opaque = true
		return
	}
	if strings.Contains(subscript, "$(") || strings.Contains(subscript, "`") || strings.Contains(subscript, "$[") {
		a.opaque = true
		subscript = strings.TrimSuffix(subscript, "]")
		a.addPayload(subscript)
	}
}

// assignedNameOperands returns the variable-name operands of a call to an
// assigning builtin (after command wrappers), as marked word texts. A
// computed word in an option position is returned as a computed name.
//
// For unset and let, which never store a string, only the subscripts of the
// returned operands matter (subscriptsOnly).
func assignedNameOperands(tokens []string) (names []string, subscriptsOnly bool) {
	rest, _, _ := unwrapCommandTokens(tokens)
	if len(rest) == 0 {
		return nil, false
	}
	args := rest[1:]
	switch filepath.Base(rest[0]) {
	case "printf":
		names := getoptNames(args, "v", "v", false)
		// An expanded first word could itself be -v; the word after it would
		// then be the name.
		if len(args) > 1 && strings.HasPrefix(args[0], rebindingMarker) {
			names = append(names, args[1])
		}
		return names, false
	case "read":
		return getoptNames(args, "adinNptu", "a", true), false
	case "mapfile", "readarray":
		names := getoptNames(args, "dnOsuCc", "", true)
		if len(names) > 1 {
			names = names[:1]
		}
		return names, false
	case "wait":
		return getoptNames(args, "p", "p", false), false
	case "getopts":
		operands := getoptNames(args, "", "", true)
		if len(operands) >= 2 {
			return operands[1:2], false
		}
		return nil, false
	case "unset", "let":
		return args, true
	}
	if declBuiltins[filepath.Base(rest[0])] {
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "+") {
				if strings.Contains(arg, rebindingMarker) {
					names = append(names, rebindingMarker)
				}
				continue
			}
			name, _, _ := strings.Cut(arg, "=")
			names = append(names, name)
		}
		return names, false
	}
	return nil, false
}

// getoptNames parses getopt-style options. valueOpts take a value (attached
// or in the next word); nameOpts are those whose value is a variable name.
// With operandsAreNames, every operand after the options is a name. A
// computed word among the options yields a computed name.
func getoptNames(args []string, valueOpts, nameOpts string, operandsAreNames bool) []string {
	var names []string
	i := 0
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if strings.Contains(arg, rebindingMarker) && strings.HasPrefix(arg, "-") {
			names = append(names, rebindingMarker)
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		for j := 1; j < len(arg); j++ {
			opt := arg[j]
			if !strings.ContainsRune(valueOpts, rune(opt)) {
				continue
			}
			value := arg[j+1:]
			if value == "" {
				if i+1 >= len(args) {
					break
				}
				i++
				value = args[i]
			}
			if strings.ContainsRune(nameOpts, rune(opt)) {
				names = append(names, value)
			}
			break
		}
	}
	if operandsAreNames {
		names = append(names, args[min(i, len(args)):]...)
	}
	return names
}

// collectAliases records every static alias definition in the script, so a
// use is classified with the alias body in place of its name wherever the
// definition appears (a loop can define an alias after its first use).
func (a *feedAnalysis) collectAliases(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		tokens := make([]string, len(call.Args))
		for i, word := range call.Args {
			value, _, ok := a.staticWord(word)
			if !ok {
				value = dynamicToken
			}
			tokens[i] = value
		}
		rest, _, _ := unwrapCommandTokens(tokens)
		if len(rest) == 0 || filepath.Base(rest[0]) != "alias" {
			return true
		}
		for _, token := range rest[1:] {
			name, body, found := strings.Cut(token, "=")
			if !found || name == "" || strings.Contains(token, dynamicToken) {
				continue
			}
			if a.aliases == nil {
				a.aliases = map[string][]string{}
			}
			a.aliases[name] = append(a.aliases[name], body)
		}
		return true
	})
}

// expandAlias returns the commands a call can run when its first word is a
// defined alias: each body followed by the call's remaining words. After a
// body ending in a blank bash also expands the next word, so `s r /x` with
// s='sudo ' and r='rm -rf' runs `sudo rm -rf /x`.
func (a *feedAnalysis) expandAlias(call *syntax.CallExpr, tokens []string) []string {
	if len(a.aliases) == 0 || len(call.Assigns) != 0 {
		return nil
	}
	tail := func(from int) string {
		var words []string
		for _, word := range call.Args[min(from, len(call.Args)):] {
			words = append(words, a.raw[word.Pos().Offset():word.End().Offset()])
		}
		return strings.Join(words, " ")
	}
	var expand func(i int, prefix string, depth int) []string
	expand = func(i int, prefix string, depth int) []string {
		bodies, ok := a.aliases[tokens[min(i, len(tokens)-1)]]
		if i >= len(tokens) || !ok || depth > maxCommandNesting {
			return nil
		}
		var out []string
		for _, body := range bodies {
			command := prefix + body
			if strings.TrimRight(body, " \t") != body {
				if more := expand(i+1, command, depth+1); len(more) > 0 {
					out = append(out, more...)
					continue
				}
			} else {
				command += " "
			}
			out = append(out, command+tail(i+1))
		}
		return out
	}
	return expand(0, "", 0)
}
