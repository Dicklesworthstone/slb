package core

import (
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// normalizeCaseCommand handles shell control flow through the AST path first
// introduced for case clauses. Splitting punctuation alone leaves "then rm",
// "do rm", or "! rm" in executable position and hides the actual command from
// anchored risk patterns. Inspect all conditions, branches and function bodies;
// do not try to predict which branch runs or whether a function will be called.
//
// The boolean reports a syntax error. The caller retains its conservative
// legacy normalization on error, so stronger risk matches are not discarded.
// Simple commands keep their existing token/wrapper normalization. In
// particular, extracted calls must not recursively re-enter this AST path.
func normalizeCaseCommand(raw string, depth int) (*NormalizedCommand, bool) {
	if depth >= maxCommandNesting || len(raw) > 1<<20 || strings.IndexByte(raw, 0) >= 0 {
		return nil, true
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(raw), "")
	if err != nil {
		return nil, true
	}

	result := &NormalizedCommand{Original: raw, Segments: []string{}}
	var executable []syntax.Node
	// Each redirection's statement, so a here-string can be judged by the
	// command that consumes it.
	redirectOwner := map[*syntax.Redirect]*syntax.Stmt{}
	var stack []bool
	nesting := depth
	hasControlFlow := false
	walkCaseSyntax(file, func(node syntax.Node) bool {
		if node == nil {
			if stack[len(stack)-1] {
				nesting--
			}
			stack = stack[:len(stack)-1]
			return true
		}
		compound := false
		switch node := node.(type) {
		case *syntax.Stmt:
			// Negation changes an exit status, not the effects of the command.
			hasControlFlow = hasControlFlow || node.Negated
			for _, redirect := range node.Redirs {
				redirectOwner[redirect] = node
			}
		case *syntax.CaseClause, *syntax.Block, *syntax.IfClause,
			*syntax.WhileClause, *syntax.ForClause, *syntax.FuncDecl,
			*syntax.TimeClause, *syntax.CoprocClause:
			hasControlFlow = true
			compound = true
		case *syntax.ArithmCmd, *syntax.TestClause, *syntax.LetClause:
			// These contain expressions rather than executable word lists,
			// but substitutions in those expressions still execute commands.
			hasControlFlow = true
		case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
			result.HasSubshell = true
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
	if !hasControlFlow {
		return nil, false
	}
	literals := collectLiteralVariables(file, raw)

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
				// cannot be cleared by otherwise benign control flow. Check
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
				text, valid := caseNodeText(raw, assign, literals)
				result.ParseError = result.ParseError || !valid
				words = append(words, text)
			}
			for _, arg := range node.Args {
				text, valid := caseNodeText(raw, arg, literals)
				result.ParseError = result.ParseError || !valid
				words = append(words, text)
			}
			appendCommand(strings.Join(words, " "), true)
		case *syntax.DeclClause:
			text, valid := caseNodeText(raw, node, literals)
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
			// Do not turn structured shell scripts into an unchecked
			// file-clobbering primitive. Discarding output is understood; other
			// file writes remain conservative until redirection risk is modeled.
			switch node.Op {
			case syntax.WordHdoc:
				// A here-string may be a program for an interpreter. Do not
				// silently drop its contents while extracting executable calls,
				// unless the command reading it only treats stdin as data
				// (`read -r a b <<< "$line"`). Substitutions inside the word are
				// visited and classified independently by the walk above.
				if !hereStringDataConsumer(redirectOwner[node]) {
					result.ParseError = true
				}
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
		// Empty cases and arithmetic/test expressions may execute no command.
		// Do not classify literal expression text as code or allowlist the script.
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

// hereStringDataConsumers read a here-string as data and cannot execute it.
// Deliberately excluded: shells and interpreters, and tools whose options or
// scripts can execute input (mapfile/readarray -C callbacks, sed's e command,
// awk's system(), xargs, sort --compress-program, tee writing files, ...).
var hereStringDataConsumers = map[string]bool{
	"read": true, "cat": true, "grep": true, "egrep": true, "fgrep": true,
	"wc": true, "head": true, "tail": true, "cut": true, "tr": true, "uniq": true,
	"rev": true, "jq": true, "base64": true,
}

func hereStringDataConsumer(stmt *syntax.Stmt) bool {
	if stmt == nil {
		return false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	// The command word itself must be a literal: a computed or wrapped
	// command could be anything.
	return hereStringDataConsumers[call.Args[0].Lit()]
}

// caseNodeText preserves literal quoting while replacing executable expansions
// with inert words. Plain references to script-local literal variables are
// replaced by their values (see collectLiteralVariables). The main AST walk
// independently visits every expansion's commands, including those in
// selectors, patterns, arithmetic and redirects.
// No shell, environment expansion or command substitution is ever executed.
func caseNodeText(raw string, node syntax.Node, literals literalVariables) (string, bool) {
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
		switch part := part.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			text = "__slb_substitution__"
		case *syntax.ArithmExp:
			text = "0"
		case *syntax.ParamExp:
			text, _ = literals.resolve(part)
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
