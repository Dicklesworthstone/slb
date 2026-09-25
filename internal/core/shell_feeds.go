package core

import (
	"encoding/base64"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Execution-feed analysis finds code that the line-oriented normalizer cannot
// see, independently of whether the command uses control flow:
//
//   - a command word that is an expansion (`X=rm; $X -rf /srv`): a provably
//     literal variable (collectLiteralVariables) is resolved and the resolved
//     command is classified; anything else is opaque;
//   - a shell or interpreter that reads its program from stdin, fed by a
//     here-string, here-doc, pipe or input redirection (`bash <<< ...`,
//     `... | sh`, `python - <<EOF`): a statically known shell payload is
//     classified recursively, anything else is opaque;
//   - a shell -c operand or eval argument built by expansion
//     (`sh -c "$(printf ...)"`): classified when statically known, else opaque.
//
// Opaque commands classify as at least CAUTION (see finalizeClassification).
// All evaluation is static: nothing is executed and no environment is read.

// maxStaticPayload bounds statically reconstructed program text.
const maxStaticPayload = 1 << 20

var stdinShells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "mksh": true, "ash": true}

// Interpreters that read a program from stdin when given no script operand
// (or "-"), mapped to the options that instead take inline code or a module.
var stdinInterpreters = map[string]map[string]bool{
	"perl":      {"-e": true, "-E": true},
	"ruby":      {"-e": true},
	"node":      {"-e": true, "-p": true, "--eval": true, "--print": true},
	"nodejs":    {"-e": true, "-p": true, "--eval": true, "--print": true},
	"php":       {"-r": true},
	"lua":       {"-e": true},
	"tclsh":     {},
	"pwsh":      {"-c": true, "-Command": true, "-command": true, "-File": true, "-file": true},
	"osascript": {"-e": true},
}

var pythonInlineFlags = map[string]bool{"-c": true, "-m": true}

func interpreterInlineFlags(name string) (map[string]bool, bool) {
	if name == "python" || strings.HasPrefix(name, "python2") || strings.HasPrefix(name, "python3") {
		return pythonInlineFlags, true
	}
	flags, ok := stdinInterpreters[name]
	return flags, ok
}

// stdinFeed describes what a statement reads on stdin.
type stdinFeed struct {
	producer *syntax.Stmt     // left side of a pipe
	redirect *syntax.Redirect // <<<, <<, <<-, <, <>, <&
}

func (f *stdinFeed) present() bool { return f != nil && (f.producer != nil || f.redirect != nil) }

type feedAnalysis struct {
	raw      string
	depth    int
	literals literalVariables
	segments []string
	opaque   bool
	parseErr bool
}

// analyzeExecutionFeeds returns extra segments to classify and whether some
// executed code could not be determined statically.
func analyzeExecutionFeeds(raw string, depth int) (segments []string, opaque, parseErr bool) {
	if depth >= maxCommandNesting || len(raw) > 1<<20 || strings.IndexByte(raw, 0) >= 0 {
		return nil, false, true
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(raw), "")
	if err != nil {
		// The line-oriented normalizer reports its own parse errors.
		return nil, false, false
	}
	a := &feedAnalysis{raw: raw, depth: depth, literals: collectLiteralVariables(file, raw)}
	for _, stmt := range file.Stmts {
		a.visitStmt(stmt, nil)
	}
	return a.segments, a.opaque, a.parseErr
}

func (a *feedAnalysis) addPayload(payload string) {
	if strings.TrimSpace(payload) == "" {
		return
	}
	inner := normalizeWithFeeds(payload, a.depth+1)
	a.segments = append(a.segments, inner.Segments...)
	a.opaque = a.opaque || inner.Opaque
	a.parseErr = a.parseErr || inner.ParseError
}

// stdinRedirect returns the last redirection of fd 0, which is what the
// command actually reads.
func stdinRedirect(redirs []*syntax.Redirect) *syntax.Redirect {
	var last *syntax.Redirect
	for _, r := range redirs {
		if r.N != nil && r.N.Value != "0" {
			continue
		}
		switch r.Op {
		case syntax.RdrIn, syntax.RdrInOut, syntax.DplIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			last = r
		}
	}
	return last
}

func (a *feedAnalysis) visitStmt(stmt *syntax.Stmt, inherited *stdinFeed) {
	if stmt == nil {
		return
	}
	feed := inherited
	if r := stdinRedirect(stmt.Redirs); r != nil {
		feed = &stdinFeed{redirect: r}
	}
	// Redirection words can contain substitutions.
	for _, r := range stmt.Redirs {
		a.visitNested(r, feed)
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		if cmd.Op == syntax.Pipe || cmd.Op == syntax.PipeAll {
			a.visitStmt(cmd.X, feed)
			a.visitStmt(cmd.Y, &stdinFeed{producer: cmd.X})
			return
		}
		a.visitStmt(cmd.X, feed)
		a.visitStmt(cmd.Y, feed)
	case *syntax.CallExpr:
		a.checkCall(cmd, feed)
		a.visitNested(cmd, feed)
	case nil:
	default:
		a.visitNested(cmd, feed)
	}
}

// visitNested finds the statements nested in node (compound bodies,
// substitutions) without descending past them; visitStmt handles each.
func (a *feedAnalysis) visitNested(node syntax.Node, feed *stdinFeed) {
	walkCaseSyntax(node, func(child syntax.Node) bool {
		if stmt, ok := child.(*syntax.Stmt); ok {
			a.visitStmt(stmt, feed)
			return false
		}
		return true
	})
}

// staticWord returns a word's value when it is provably literal, resolving
// plain references to script-local literal variables.
func (a *feedAnalysis) staticWord(word *syntax.Word) (value string, resolved, ok bool) {
	if word == nil {
		return "", false, false
	}
	var out strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			text := part.Value
			// Tilde expansion names a home directory, not computed code; the
			// word is kept verbatim (the pattern engine resolves ~ itself).
			if unquotedGlob(text) {
				return "", false, false // pathname expansion
			}
			out.WriteString(unescapeUnquoted(text))
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", false, false
			}
			out.WriteString(part.Value)
		case *syntax.DblQuoted:
			if part.Dollar {
				return "", false, false
			}
			for _, inner := range part.Parts {
				switch inner := inner.(type) {
				case *syntax.Lit:
					out.WriteString(unescapeDoubleQuoted(inner.Value))
				case *syntax.ParamExp:
					text, ok := a.literals.resolve(inner)
					if !ok {
						return "", false, false
					}
					resolved = true
					out.WriteString(text)
				default:
					return "", false, false
				}
			}
		case *syntax.ParamExp:
			text, ok := a.literals.resolve(part)
			if !ok {
				return "", false, false
			}
			resolved = true
			out.WriteString(text)
		default:
			return "", false, false
		}
	}
	return out.String(), resolved, true
}

// unquotedGlob reports pathname-expansion syntax: * or ?, or a bracket
// expression (a lone "[" such as the test command is not one).
func unquotedGlob(s string) bool {
	if strings.ContainsAny(s, "*?") {
		return true
	}
	open := strings.IndexByte(s, '[')
	return open >= 0 && strings.IndexByte(s[open+1:], ']') >= 0
}

func firstLit(word *syntax.Word) (string, bool) {
	if word == nil || len(word.Parts) == 0 {
		return "", false
	}
	lit, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return "", false
	}
	return lit.Value, true
}

func unescapeUnquoted(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func unescapeDoubleQuoted(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// dynamicProgramWord reports a word whose value is produced by running code.
func dynamicProgramWord(word *syntax.Word) bool {
	found := false
	walkCaseSyntax(word, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			found = true
			return false
		}
		return true
	})
	return found
}

const dynamicToken = "\x00dynamic"

func (a *feedAnalysis) checkCall(call *syntax.CallExpr, feed *stdinFeed) {
	if len(call.Args) == 0 {
		return
	}
	tokens := make([]string, len(call.Args))
	anyResolved := false
	for i, word := range call.Args {
		value, resolved, ok := a.staticWord(word)
		if !ok {
			tokens[i] = dynamicToken
			// Keep an env-style NAME=value argument recognizable (as in
			// `env CARGO_TARGET_DIR="$S/target" cargo ...`): only its value
			// is computed, not the executed program.
			if lit, isLit := firstLit(word); isLit {
				if name := envAssignPattern.FindString(lit); name != "" {
					tokens[i] = name + dynamicToken
				}
			}
			continue
		}
		tokens[i] = value
		anyResolved = anyResolved || resolved
	}
	if anyResolved {
		// Classify the command as it will actually run.
		var words []string
		valid := true
		for _, assign := range call.Assigns {
			text, ok := caseNodeText(a.raw, assign, a.literals)
			valid = valid && ok
			words = append(words, text)
		}
		for _, word := range call.Args {
			text, ok := caseNodeText(a.raw, word, a.literals)
			valid = valid && ok
			words = append(words, text)
		}
		if !valid {
			a.parseErr = true
		}
		a.addPayload(strings.Join(words, " "))
	}

	rest, _, _ := unwrapCommandTokens(tokens)
	if len(rest) == 0 {
		return
	}
	offset := len(tokens) - len(rest)
	if strings.Contains(rest[0], dynamicToken) {
		a.opaque = true // the executed program is computed
		return
	}
	name := filepath.Base(rest[0])
	args := call.Args[offset:]
	switch {
	case name == "eval":
		// eval parses its expanded arguments as code: an unknown value is
		// unknown code.
		for _, token := range rest[1:] {
			if strings.Contains(token, dynamicToken) {
				a.opaque = true
				return
			}
		}
		a.addPayload(strings.Join(rest[1:], " "))
	case stdinShells[name]:
		a.checkShell(rest, args, feed)
	default:
		if inline, ok := interpreterInlineFlags(name); ok {
			a.checkInterpreter(rest, args, inline, feed)
		}
	}
}

func (a *feedAnalysis) checkShell(tokens []string, words []*syntax.Word, feed *stdinFeed) {
	// With -s the program is read from stdin and operands are its $1...
	stdinFlag := false
	for i := 1; i < len(tokens); i++ {
		if tokens[i] == "--" {
			if stdinFlag || i+1 == len(tokens) {
				break
			}
			return // script operand follows
		}
		arg := tokens[i]
		if strings.Contains(arg, dynamicToken) && stdinFlag {
			break // an argument of the stdin program
		}
		if strings.Contains(arg, dynamicToken) {
			// A computed option or operand: the program source is unknown
			// when it is produced by running code.
			if dynamicProgramWord(words[i]) {
				a.opaque = true
			}
			return
		}
		if arg == "-" {
			break // explicit stdin program
		}
		if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "+") {
			if stdinFlag {
				break // arguments of the stdin program
			}
			return // script file operand: its contents are not visible here
		}
		if arg == "-o" || arg == "-O" || arg == "+o" || arg == "+O" || arg == "--rcfile" || arg == "--init-file" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--") {
			continue
		}
		if strings.ContainsRune(arg[1:], 'c') {
			if i+1 >= len(tokens) {
				return
			}
			if !strings.Contains(tokens[i+1], dynamicToken) {
				// Literal bodies are also normalized line-by-line; this adds
				// the feeds hidden inside them.
				a.addPayload(tokens[i+1])
				return
			}
			if payload, ok := a.substitutionOutput(words[i+1]); ok {
				a.addPayload(payload)
				return
			}
			// The outer shell expands parameters before the inner shell parses
			// the body, so an unknown value is unknown code, not data.
			a.opaque = true
			return
		}
		if !strings.HasPrefix(arg, "--") && strings.ContainsRune(arg[1:], 's') {
			stdinFlag = true
		}
	}
	a.checkStdinProgram(feed, true)
}

func (a *feedAnalysis) checkInterpreter(tokens []string, words []*syntax.Word, inline map[string]bool, feed *stdinFeed) {
	for i := 1; i < len(tokens); i++ {
		arg := tokens[i]
		if strings.Contains(arg, dynamicToken) {
			if dynamicProgramWord(words[i]) {
				a.opaque = true
			}
			return
		}
		if arg == "-" {
			break
		}
		if inline[arg] || (len(arg) > 2 && inline[arg[:2]] && !strings.HasPrefix(arg, "--")) {
			return // inline code or a module, not a stdin program
		}
		if !strings.HasPrefix(arg, "-") {
			return // script file operand
		}
	}
	a.checkStdinProgram(feed, false)
}

// checkStdinProgram handles a shell/interpreter whose program is its stdin.
// Without a feed it reads the caller's terminal, which is not part of this
// command.
func (a *feedAnalysis) checkStdinProgram(feed *stdinFeed, shell bool) {
	if !feed.present() {
		return
	}
	payload, ok := a.feedOutput(feed)
	if !ok || !shell {
		// Non-shell programs cannot be judged by shell risk patterns.
		a.opaque = true
		return
	}
	a.addPayload(payload)
}

func (a *feedAnalysis) feedOutput(feed *stdinFeed) (string, bool) {
	if feed.redirect != nil {
		return a.redirectInput(feed.redirect)
	}
	return a.stmtOutput(feed.producer, nil, 0)
}

// redirectInput returns the data a stdin redirection supplies, if static.
func (a *feedAnalysis) redirectInput(r *syntax.Redirect) (string, bool) {
	switch r.Op {
	case syntax.WordHdoc:
		value, _, ok := a.staticWord(r.Word)
		if !ok {
			return "", false
		}
		return value + "\n", true
	case syntax.Hdoc, syntax.DashHdoc:
		if r.Hdoc == nil {
			return "", true
		}
		quoted := false
		for _, part := range r.Word.Parts {
			switch part := part.(type) {
			case *syntax.SglQuoted, *syntax.DblQuoted:
				quoted = true
			case *syntax.Lit:
				quoted = quoted || strings.Contains(part.Value, `\`)
			}
		}
		var out strings.Builder
		for _, part := range r.Hdoc.Parts {
			lit, ok := part.(*syntax.Lit)
			if !ok {
				return "", false // expansions in an unquoted here-doc
			}
			if !quoted && strings.Contains(lit.Value, `\`) {
				return "", false
			}
			out.WriteString(lit.Value)
		}
		return out.String(), true
	}
	return "", false // files and descriptors are not visible here
}

// substitutionOutput evaluates a word that is exactly one command
// substitution (optionally double-quoted) with a static result.
func (a *feedAnalysis) substitutionOutput(word *syntax.Word) (string, bool) {
	if word == nil || len(word.Parts) != 1 {
		return "", false
	}
	part := word.Parts[0]
	if dq, ok := part.(*syntax.DblQuoted); ok {
		if len(dq.Parts) != 1 {
			return "", false
		}
		part = dq.Parts[0]
	}
	subst, ok := part.(*syntax.CmdSubst)
	if !ok || len(subst.Stmts) != 1 {
		return "", false
	}
	out, ok := a.stmtOutput(subst.Stmts[0], nil, 0)
	if !ok {
		return "", false
	}
	return strings.TrimRight(out, "\n"), true
}

// stmtOutput statically evaluates the stdout of a small set of data-only
// commands (echo, printf, cat, base64 -d, true) and pipelines of them.
// input is the known stdin, or nil when there is none or it is unknown.
func (a *feedAnalysis) stmtOutput(stmt *syntax.Stmt, input *string, depth int) (string, bool) {
	if stmt == nil || depth > maxCommandNesting || stmt.Negated || stmt.Background || stmt.Coprocess {
		return "", false
	}
	for _, r := range stmt.Redirs {
		if r.N != nil && r.N.Value != "0" && r.N.Value != "1" {
			continue // stderr and other descriptors do not change stdout
		}
		switch r.Op {
		case syntax.RdrIn, syntax.RdrInOut, syntax.DplIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		default:
			return "", false // stdout diverted
		}
	}
	if r := stdinRedirect(stmt.Redirs); r != nil {
		data, ok := a.redirectInput(r)
		if !ok {
			input = nil
		} else {
			input = &data
		}
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		if cmd.Op != syntax.Pipe && cmd.Op != syntax.PipeAll {
			return "", false
		}
		left, ok := a.stmtOutput(cmd.X, input, depth+1)
		if !ok {
			return "", false
		}
		return a.stmtOutput(cmd.Y, &left, depth+1)
	case *syntax.Subshell:
		if len(cmd.Stmts) != 1 {
			return "", false
		}
		return a.stmtOutput(cmd.Stmts[0], input, depth+1)
	case *syntax.Block:
		if len(cmd.Stmts) != 1 {
			return "", false
		}
		return a.stmtOutput(cmd.Stmts[0], input, depth+1)
	case *syntax.CallExpr:
		if len(cmd.Assigns) != 0 || len(cmd.Args) == 0 {
			return "", false
		}
		args := make([]string, len(cmd.Args))
		for i, word := range cmd.Args {
			value, _, ok := a.staticWord(word)
			if !ok {
				return "", false
			}
			args[i] = value
		}
		out, ok := staticCommandOutput(args, input)
		if !ok || len(out) > maxStaticPayload {
			return "", false
		}
		return out, true
	}
	return "", false
}

func staticCommandOutput(args []string, input *string) (string, bool) {
	switch filepath.Base(args[0]) {
	case "true", ":":
		return "", true
	case "echo":
		newline := true
		i := 1
		for ; i < len(args); i++ {
			flag := args[i]
			if len(flag) < 2 || flag[0] != '-' || strings.Trim(flag[1:], "neE") != "" {
				break
			}
			if strings.Contains(flag, "n") {
				newline = false
			}
			if strings.Contains(flag, "e") {
				for _, rest := range args[i+1:] {
					if strings.Contains(rest, `\`) {
						return "", false
					}
				}
			}
		}
		out := strings.Join(args[i:], " ")
		if newline {
			out += "\n"
		}
		return out, true
	case "printf":
		if len(args) < 2 {
			return "", false
		}
		format := args[1]
		switch {
		case !strings.Contains(format, "%") && len(args) == 2:
			return printfEscapes(format)
		case format == "%s" || format == `%s\n`:
			suffix := ""
			if format == `%s\n` {
				suffix = "\n"
			}
			values := args[2:]
			if len(values) == 0 {
				values = []string{""}
			}
			var out strings.Builder
			for _, value := range values {
				out.WriteString(value + suffix)
			}
			return out.String(), true
		}
		return "", false
	case "cat":
		if len(args) > 2 || (len(args) == 2 && args[1] != "-") || input == nil {
			return "", false
		}
		return *input, true
	case "base64":
		decode := false
		for _, flag := range args[1:] {
			switch flag {
			case "-d", "-D", "--decode":
				decode = true
			case "-i", "--ignore-garbage":
			default:
				return "", false
			}
		}
		if !decode || input == nil {
			return "", false
		}
		clean := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, *input)
		decoded, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(clean)
			if err != nil {
				return "", false
			}
		}
		return string(decoded), true
	}
	return "", false
}

func printfEscapes(format string) (string, bool) {
	if !strings.Contains(format, `\`) {
		return format, true
	}
	var out strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '\\' {
			out.WriteByte(format[i])
			continue
		}
		if i+1 >= len(format) {
			return "", false
		}
		i++
		switch format[i] {
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case '\\':
			out.WriteByte('\\')
		case '"', '\'':
			out.WriteByte(format[i])
		default:
			return "", false // octal/hex escapes are not modeled
		}
	}
	return out.String(), true
}
