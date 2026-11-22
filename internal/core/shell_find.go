package core

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// findPathPlaceholder is an unknown pathname substituted by find. It must
// never make an executed command eligible for a SAFE argument allowlist.
const findPathPlaceholder = "__slb_find_path__"

// Expression primaries consume their operands even when an operand spells an
// action: `-name -delete` searches for that name and does not delete anything.
// Unknown syntax stays opaque instead of guessing which words are actions.
var findValuePrimaries = map[string]bool{
	"-name": true, "-iname": true, "-path": true, "-ipath": true,
	"-wholename": true, "-iwholename": true, "-regex": true, "-iregex": true,
	"-lname": true, "-ilname": true, "-type": true, "-xtype": true,
	"-perm": true, "-user": true, "-uid": true, "-group": true, "-gid": true,
	"-mtime": true, "-mmin": true, "-ctime": true, "-cmin": true,
	"-atime": true, "-amin": true, "-newer": true, "-anewer": true, "-cnewer": true,
	"-links": true, "-inum": true, "-samefile": true, "-size": true, "-used": true,
	"-fstype": true, "-context": true, "-maxdepth": true, "-mindepth": true,
	"-regextype": true, "-printf": true,
}

var findNoValuePrimaries = map[string]bool{
	"-true": true, "-false": true, "-print": true, "-print0": true, "-ls": true,
	"-prune": true, "-quit": true, "-empty": true, "-readable": true,
	"-writable": true, "-executable": true, "-nouser": true, "-nogroup": true,
	"-depth": true, "-daystart": true, "-follow": true, "-mount": true, "-xdev": true,
	"-ignore_readdir_race": true, "-noignore_readdir_race": true, "-noleaf": true,
	"(": true, ")": true, "!": true, ",": true,
	"-a": true, "-and": true, "-o": true, "-or": true, "-not": true,
}

// checkFind inspects actions without walking the filesystem or executing find.
// Predicates are not a proof of a bounded set of matches. Deletion is analyzed
// as recursive removal below every starting point, while -exec/-ok payloads
// are reconstructed as argv (not concatenated shell code) and analyzed using
// the normal execution-feed machinery. The original command remains intact.
func (a *feedAnalysis) checkFind(tokens []string, words []*syntax.Word) {
	var roots []string
	payloadBudget := maxStaticPayload
	i := 1
	for i < len(tokens) {
		arg := tokens[i]
		switch arg {
		case "-H", "-L", "-P", "-E", "-X", "-s", "-x", "-d", "-O0", "-O1", "-O2", "-O3":
			i++
			continue
		case "-D", "-f":
			if i+1 == len(tokens) {
				a.opaque = true
				return
			}
			if arg == "-f" { // BSD's explicit starting-point option
				roots = append(roots, a.findStartingPoint(tokens[i+1], words[i+1]))
			}
			a.opaque = a.opaque || strings.Contains(tokens[i+1], dynamicToken)
			i += 2
			continue
		}
		break
	}
	for i < len(tokens) && !strings.HasPrefix(tokens[i], "-") && tokens[i] != "(" && tokens[i] != "!" {
		roots = append(roots, a.findStartingPoint(tokens[i], words[i]))
		a.opaque = a.opaque || strings.Contains(tokens[i], dynamicToken)
		i++
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}
	for ; i < len(tokens); i++ {
		arg := tokens[i]
		switch {
		case arg == "-delete":
			payload := []string{"rm", "-r", "--"}
			for _, root := range roots {
				payload = append(payload, strings.ReplaceAll(root, dynamicToken, findPathPlaceholder))
			}
			if !a.addFindPayload(payload, &payloadBudget) {
				return
			}
		case arg == "-exec" || arg == "-execdir" || arg == "-ok" || arg == "-okdir":
			// Matching files, substitutions and per-directory execution are
			// not a fixed invocation. Even an outer SAFE find rule must wait
			// for review; a known destructive payload retains its higher tier.
			a.opaque = true
			start := i + 1
			end := start
			for end < len(tokens) {
				if tokens[end] == ";" || (tokens[end] == "+" && end > start && tokens[end-1] == "{}" && (arg == "-exec" || arg == "-execdir")) {
					break
				}
				end++
			}
			if end == len(tokens) || end == start {
				return // missing executable or terminator
			}
			payload := make([]string, end-start)
			for n, word := range tokens[start:end] {
				word = strings.ReplaceAll(word, dynamicToken, findPathPlaceholder)
				payload[n] = strings.ReplaceAll(word, "{}", findPathPlaceholder)
			}
			if !a.addFindPayload(payload, &payloadBudget) || !a.findRMRootEffects(payload, roots, arg == "-execdir" || arg == "-okdir", &payloadBudget) {
				return
			}
			i = end
		case arg == "-fprint" || arg == "-fprint0" || arg == "-fls" || arg == "-fprintf":
			arity := 1
			if arg == "-fprintf" {
				arity = 2
			}
			if i+arity >= len(tokens) {
				a.opaque = true
				return
			}
			// These actions create/truncate the file even if no path matches.
			output := tokens[i+1]
			a.writesFiles = a.writesFiles || (output != "/dev/null" && output != "/dev/stdout" && output != "/dev/stderr")
			i += arity
		case arg == "-files0-from":
			// The file (or stdin) is not inspected during classification.
			a.opaque = true
			if i+1 == len(tokens) {
				return
			}
			roots = []string{findPathPlaceholder}
			i++
		case findValuePrimaries[arg] || (strings.HasPrefix(arg, "-newer") && len(arg) == 8):
			if i+1 == len(tokens) {
				a.opaque = true
				return
			}
			a.opaque = a.opaque || strings.Contains(tokens[i+1], dynamicToken)
			i++
		case findNoValuePrimaries[arg]:
			continue
		default:
			a.opaque = true
			return
		}
	}
}

// Repeated actions must not multiply a long starting-point list into unbounded
// generated work. The existing parse-error upgrade keeps exhausted analysis
// conservative while retaining any stronger risk already discovered.
func (a *feedAnalysis) addFindPayload(tokens []string, budget *int) bool {
	payload := shellJoin(tokens)
	if len(payload) > *budget {
		a.parseErr = true
		a.opaque = true
		return false
	}
	*budget -= len(payload)
	a.addPayload(payload)
	return true
}

// Retain a known path prefix around expansion, so /etc/$NAME and /etc/* do
// not lose their protected-directory risk. A plain HOME reference at a path
// boundary is symbolic, like the rm normalizer; no environment is expanded.
func (a *feedAnalysis) findStartingPoint(token string, word *syntax.Word) string {
	if !strings.Contains(token, dynamicToken) {
		return token
	}
	text := a.markedWordText(word)
	if param, ok := firstFindPathPart(word.Parts).(*syntax.ParamExp); ok && param.Param != nil && param.Param.Value == "HOME" && plainParamExp(param) {
		if text == rebindingMarker || strings.HasPrefix(text, rebindingMarker+"/") {
			text = "~" + strings.TrimPrefix(text, rebindingMarker)
		}
	}
	return strings.ReplaceAll(text, rebindingMarker, findPathPlaceholder)
}

func firstFindPathPart(parts []syntax.WordPart) syntax.WordPart {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if unescapeUnquoted(p.Value) == "" {
				continue
			}
		case *syntax.SglQuoted:
			if !p.Dollar && p.Value == "" {
				continue
			}
		case *syntax.DblQuoted:
			if !p.Dollar {
				if first := firstFindPathPart(p.Parts); first != nil {
					return first
				}
				continue
			}
		}
		return part
	}
	return nil
}

// rm of a matched filename must retain the starting point's risk. -execdir
// additionally changes cwd to the matched path's containing directory. Bound
// relative operands at the starting point's parent and at the starting point
// (for descendants); deeper descendants cannot traverse higher with the same
// operand. Keep both the unknown filename and these conservative path views.
// This is one additional argv analysis, bounded in size, never one recursive
// analysis for every possible match. Other executables keep their opaque floor.
func (a *feedAnalysis) findRMRootEffects(payload, roots []string, perDirectory bool, budget *int) bool {
	command, _, valid := unwrapCommandTokens(payload)
	if !valid || len(command) < 2 || filepath.Base(command[0]) != "rm" {
		return true
	}
	var extra []string
	size := len(shellJoin(command))
	add := func(path string) bool {
		size += len(shellQuote(path)) + 1
		if size > *budget {
			a.parseErr = true
			return false
		}
		extra = append(extra, path)
		return true
	}
	endOptions := false
	for _, operand := range command[1:] {
		if !endOptions && operand == "--" {
			endOptions = true
			continue
		}
		if !endOptions && strings.HasPrefix(operand, "-") && operand != "-" {
			continue
		}
		substituted := strings.Contains(operand, findPathPlaceholder)
		relative := perDirectory && operand != "" && !filepath.IsAbs(operand)
		if !substituted && !relative {
			continue
		}
		for _, root := range roots {
			if substituted {
				if !add(root) || !add(strings.ReplaceAll(operand, findPathPlaceholder, root)) {
					return false
				}
			} else {
				if strings.HasPrefix(root, "~") || strings.Contains(root, findPathPlaceholder) {
					// The execution directory cannot be bounded more narrowly.
					if !add("/") {
						return false
					}
				} else if !add(filepath.Join(filepath.Dir(root), operand)) || !add(filepath.Join(root, operand)) {
					return false
				}
			}
		}
	}
	if len(extra) == 0 {
		return true
	}
	bounded := append([]string{"rm"}, command[1:]...)
	if !endOptions {
		bounded = append(bounded, "--")
	}
	bounded = append(bounded, extra...)
	// Lexical path cleaning can erase a marker inside {}/../... Keep a
	// standalone one so this contextual view cannot acquire a SAFE exemption.
	return a.addFindPayload(append(bounded, findPathPlaceholder), budget)
}
