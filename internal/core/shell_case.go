package core

import (
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// normalizeCaseCommand uses shell grammar only for scripts containing a case
// clause. A case pattern's ')' is not a subshell delimiter, and its '|' is not
// a pipeline. Removing those characters without parsing the surrounding shell
// would both accept malformed commands and hide executable arms (GitHub #16).
//
// The boolean reports a syntax error. The caller retains its conservative
// legacy normalization on error, so stronger risk matches are not discarded.
// A nil result without an error leaves non-case commands on their existing path.
func normalizeCaseCommand(raw string, depth int) (*NormalizedCommand, bool) {
	if !strings.Contains(raw, "case") && !strings.Contains(strings.ReplaceAll(raw, "\\\n", ""), "case") {
		return nil, false
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return nil, true
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(raw), "")
	if err != nil {
		return nil, true
	}

	result := &NormalizedCommand{Original: raw, Segments: []string{}}
	var executable []syntax.Node
	var stack []bool
	nesting := depth
	hasCase := false
	walkCaseSyntax(file, func(node syntax.Node) bool {
		if node == nil {
			if stack[len(stack)-1] {
				nesting--
			}
			stack = stack[:len(stack)-1]
			return true
		}
		compound := false
		switch node.(type) {
		case *syntax.CaseClause:
			hasCase = true
			compound = true
		case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
			result.HasSubshell = true
			compound = true
		case *syntax.Block, *syntax.IfClause, *syntax.WhileClause, *syntax.ForClause, *syntax.FuncDecl:
			compound = true
		case *syntax.CallExpr, *syntax.DeclClause, *syntax.Redirect:
			executable = append(executable, node)
		case *syntax.ExtGlob:
			// The parser stores an extglob's pattern as opaque text, not
			// expansion nodes. Literal extglobs are supported; dynamic ones
			// must not hide executable expansions from classification.
			if strings.ContainsAny(raw[node.Pos().Offset():node.End().Offset()], "$`") {
				result.ParseError = true
			}
		}
		if compound && nesting+1 >= maxCommandNesting {
			result.ParseError = true
			return false
		}
		stack = append(stack, compound)
		if compound {
			nesting++
		}
		return true
	})
	if !hasCase {
		return nil, false
	}

	appendCommand := func(command string, executable bool) {
		inner := normalizeCommandDepth(command, depth+1)
		result.Segments = append(result.Segments, inner.Segments...)
		result.StrippedWrappers = append(result.StrippedWrappers, inner.StrippedWrappers...)
		result.HasSubshell = result.HasSubshell || inner.HasSubshell
		result.ParseError = result.ParseError || inner.ParseError
		if executable {
			for _, segment := range inner.Segments {
				words := strings.Fields(segment)
				if len(words) == 0 {
					continue
				}
				// A computed executable or an unmodeled shell-code loader
				// cannot be cleared by an otherwise benign case arm. Check
				// after wrapper removal so sudo/command/builtin cannot hide it.
				name := words[0]
				if strings.ContainsAny(name, "$`*?{") || (strings.Contains(name, "[") && strings.Contains(name, "]")) ||
					strings.Contains(name, "__slb_substitution__") ||
					name == "eval" || name == "source" || name == "." || name == "exec" {
					result.ParseError = true
				}
			}
		}
	}
	for _, node := range executable {
		switch node := node.(type) {
		case *syntax.CallExpr:
			// Build from words, not the call's source range: redirections can
			// occur between arguments but are not children of CallExpr.
			var words []string
			for _, assign := range node.Assigns {
				text, valid := caseNodeText(raw, assign)
				result.ParseError = result.ParseError || !valid
				words = append(words, text)
			}
			for _, arg := range node.Args {
				text, valid := caseNodeText(raw, arg)
				result.ParseError = result.ParseError || !valid
				words = append(words, text)
			}
			appendCommand(strings.Join(words, " "), true)
		case *syntax.DeclClause:
			text, valid := caseNodeText(raw, node)
			result.ParseError = result.ParseError || !valid
			appendCommand(text, true)
		case *syntax.Redirect:
			if node.Hdoc != nil {
				// Preserve the quoted-heredoc policy: literal body lines stay
				// visible to risk patterns, while expanding heredocs fail closed.
				header := raw[node.OpPos.Offset():node.Word.End().Offset()]
				_, seen, supported := quotedHeredocsInLine(header)
				result.ParseError = result.ParseError || !seen || !supported
				body := raw[node.Hdoc.Pos().Offset():node.Hdoc.End().Offset()]
				for _, line := range strings.Split(body, "\n") {
					if strings.TrimSpace(line) != "" {
						appendCommand(shellQuote(line), false)
					}
				}
			}
			// Do not turn previously blocked case scripts into an unchecked
			// file-clobbering primitive. Discarding output is understood; other
			// file writes remain conservative until redirection risk is modeled.
			switch node.Op {
			case syntax.WordHdoc:
				// A here-string may be a program for an interpreter. Do not
				// silently drop its contents while extracting executable calls.
				result.ParseError = true
			case syntax.DplIn, syntax.DplOut:
				// >&word is also Bash's legacy file-output syntax. Only
				// literal descriptor duplication, closing and moving are known.
				target := raw[node.Word.Pos().Offset():node.Word.End().Offset()]
				fd := strings.TrimSuffix(target, "-")
				if target != "-" && (fd == "" || strings.Trim(fd, "0123456789") != "") {
					result.ParseError = true
				}
			case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.ClbOut, syntax.RdrInOut:
				target := raw[node.Word.Pos().Offset():node.Word.End().Offset()]
				if target != "/dev/null" && target != "'/dev/null'" && target != `"/dev/null"` {
					result.ParseError = true
				}
			}
		}
	}
	if len(result.Segments) == 0 {
		// Empty cases and arms execute no command. Do not classify literal
		// selector/pattern text as code, or mark the entire script allowlisted.
		result.Segments = []string{":"}
	}
	result.Primary = result.Segments[0]
	result.IsCompound = len(result.Segments) > 1
	return result, false
}

// walkCaseSyntax includes parameter-slice arithmetic. The pinned syntax.Walk
// traverses ParamExp.Index/Repl/Exp but omits Slice.Offset and Slice.Length,
// which can also contain command substitutions (including in case selectors).
func walkCaseSyntax(root syntax.Node, visit func(syntax.Node) bool) {
	syntax.Walk(root, func(node syntax.Node) bool {
		if !visit(node) {
			return false
		}
		if param, ok := node.(*syntax.ParamExp); ok && param.Slice != nil {
			if param.Slice.Offset != nil {
				walkCaseSyntax(param.Slice.Offset, visit)
			}
			if param.Slice.Length != nil {
				walkCaseSyntax(param.Slice.Length, visit)
			}
		}
		return true
	})
}

// caseNodeText preserves literal quoting while replacing executable expansions
// with inert words. The main AST walk independently visits every expansion's
// commands, including those in selectors, patterns, arithmetic and redirects.
// No shell, environment expansion or command substitution is ever executed.
func caseNodeText(raw string, node syntax.Node) (string, bool) {
	start, end := node.Pos().Offset(), node.End().Offset()
	if start > end || end > uint(len(raw)) {
		return "", false
	}
	valid := true
	type replacement struct {
		start, end uint
		text       string
	}
	var replacements []replacement
	walkCaseSyntax(node, func(part syntax.Node) bool {
		if part == nil {
			return true
		}
		text := ""
		switch part.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			text = "__slb_substitution__"
		case *syntax.ArithmExp:
			text = "0"
		}
		if text == "" {
			return true
		}
		lo, hi := part.Pos().Offset(), part.End().Offset()
		if lo < start || hi < lo || hi > end {
			valid = false
			return false
		}
		replacements = append(replacements, replacement{lo, hi, text})
		return false
	})
	// AST traversal order need not be source order (e.g. assignment indexes
	// versus values, or supplemental slice expressions).
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
	var out strings.Builder
	cursor := start
	for _, item := range replacements {
		if item.start < cursor {
			return "", false
		}
		out.WriteString(raw[cursor:item.start])
		out.WriteString(item.text)
		cursor = item.end
	}
	out.WriteString(raw[cursor:end])
	return out.String(), valid
}
