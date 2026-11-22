package core

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mattn/go-shellwords"
)

// cloudOptionSpec describes the argv consumed by one known option. Optional
// values use Default and never consume the next word: kubectl's bare --cascade
// leaves the following resource type in place. The offline classifier exports
// these tables so its narrower, literal-only parser uses the same arities.
type cloudOptionSpec struct {
	Value       bool   `json:"value"`
	Separate    bool   `json:"separate"`
	Equals      bool   `json:"equals"`
	Attached    bool   `json:"attached"`
	Default     string `json:"default"`
	Opaque      bool   `json:"opaque"`
	Information bool   `json:"information"`
}

var (
	cloudStringOption = cloudOptionSpec{Value: true, Separate: true, Equals: true}
	cloudShortOption  = cloudOptionSpec{Value: true, Separate: true, Equals: true, Attached: true}
	cloudBoolOption   = cloudOptionSpec{Equals: true}
	cloudHelpOption   = cloudOptionSpec{Equals: true, Information: true}
	cloudConfigOption = cloudOptionSpec{Value: true, Separate: true, Equals: true, Opaque: true}
)

var cloudGlobalOptions = map[string]map[string]cloudOptionSpec{
	"kubectl": {
		"--namespace": cloudStringOption, "-n": cloudShortOption,
		"--context": cloudStringOption, "--cluster": cloudStringOption, "--user": cloudStringOption,
		"--server": cloudStringOption, "-s": cloudShortOption,
		"--kubeconfig": cloudConfigOption, "--kuberc": cloudConfigOption,
		"--cache-dir": cloudStringOption, "--certificate-authority": cloudStringOption,
		"--client-certificate": cloudStringOption, "--client-key": cloudStringOption,
		"--token": cloudStringOption, "--username": cloudStringOption, "--password": cloudStringOption,
		"--as": cloudStringOption, "--as-uid": cloudStringOption, "--as-group": cloudStringOption,
		"--as-user-extra": cloudStringOption, "--tls-server-name": cloudStringOption,
		"--request-timeout": cloudStringOption, "--proxy-url": cloudStringOption,
		"--insecure-skip-tls-verify": cloudBoolOption, "--disable-compression": cloudBoolOption,
		"--match-server-version": cloudBoolOption, "--warnings-as-errors": cloudBoolOption,
		"-v": cloudShortOption, "--v": cloudStringOption, "--vmodule": cloudStringOption,
		"--help": cloudHelpOption, "-h": cloudHelpOption,
	},
	"helm": {
		"--namespace": cloudStringOption, "-n": cloudShortOption,
		"--kube-context": cloudStringOption, "--kubeconfig": cloudConfigOption,
		"--kube-apiserver": cloudStringOption, "--kube-as-user": cloudStringOption,
		"--kube-as-group": cloudStringOption, "--kube-ca-file": cloudStringOption,
		"--kube-tls-server-name": cloudStringOption, "--kube-token": cloudStringOption,
		"--kube-insecure-skip-tls-verify": cloudBoolOption, "--debug": cloudBoolOption,
		"--burst-limit": cloudStringOption, "--qps": cloudStringOption,
		"--registry-config": cloudConfigOption, "--repository-config": cloudConfigOption,
		"--repository-cache": cloudStringOption,
		"--help":             cloudHelpOption, "-h": cloudHelpOption,
	},
	"terraform": {
		// Terraform requires -chdir=DIR; it does not consume a separate word.
		"-chdir": {Value: true, Equals: true},
		"-help":  {Information: true}, "--help": {Information: true}, "-h": {Information: true},
		"-version": {Information: true}, "--version": {Information: true}, "-v": {Information: true},
	},
}

var kubectlDeleteOptions = map[string]cloudOptionSpec{
	"--all": cloudBoolOption, "--all-namespaces": cloudBoolOption, "-A": cloudBoolOption,
	"--force": cloudBoolOption, "--now": cloudBoolOption, "--wait": cloudBoolOption,
	"--ignore-not-found": cloudBoolOption, "--interactive": cloudBoolOption, "-i": cloudBoolOption,
	"--recursive": cloudBoolOption, "-R": cloudBoolOption,
	"--cascade":      {Value: true, Equals: true, Default: "background"},
	"--dry-run":      {Value: true, Equals: true, Default: "unchanged"},
	"--grace-period": cloudStringOption, "--timeout": cloudStringOption,
	"--selector": cloudStringOption, "-l": cloudShortOption, "--field-selector": cloudStringOption,
	"--output": cloudStringOption, "-o": cloudShortOption,
	// These select resources not represented by the visible resource operands.
	// In particular, an explicit pod argument cannot authorize a manifest too.
	"--filename": cloudConfigOption, "-f": {Value: true, Separate: true, Equals: true, Attached: true, Opaque: true},
	"--kustomize": cloudConfigOption, "-k": {Value: true, Separate: true, Equals: true, Attached: true, Opaque: true},
	"--raw": cloudConfigOption,
}

var terraformExecutionOptions = map[string]cloudOptionSpec{
	"-auto-approve": cloudBoolOption, "-input": cloudBoolOption, "-lock": cloudBoolOption,
	"-refresh": cloudBoolOption, "-destroy": cloudBoolOption, "-refresh-only": cloudBoolOption,
	"-json": cloudBoolOption, "-no-color": cloudBoolOption, "-compact-warnings": cloudBoolOption,
	"-target": cloudStringOption, "-var": cloudStringOption, "-var-file": cloudStringOption,
	"-parallelism": cloudStringOption, "-lock-timeout": cloudStringOption,
	"-state": cloudStringOption, "-state-out": cloudStringOption, "-backup": cloudStringOption,
	"-replace": cloudStringOption,
	"-help":    {Information: true}, "--help": {Information: true}, "-h": {Information: true},
}

var kubectlProtectedResources = map[string]string{
	"node": "node", "nodes": "node", "no": "node",
	"namespace": "namespace", "namespaces": "namespace", "ns": "namespace",
	"pv": "pv", "persistentvolume": "pv", "persistentvolumes": "pv",
	"pvc": "pvc", "pvcs": "pvc", "persistentvolumeclaim": "pvc", "persistentvolumeclaims": "pvc",
}

type cloudParsedOption struct {
	name        string
	value       string
	next        int
	opaque      bool
	information bool
}

type cloudOptionState struct {
	opaque               bool
	information          string
	informationUncertain bool
}

func (s *cloudOptionState) observe(option cloudParsedOption) {
	s.opaque = s.opaque || option.opaque
	s.informationUncertain = s.informationUncertain || optionTokenIsDynamic(option.value)
	if option.information {
		// Cobra/pflag keeps the last Boolean value, including across the
		// subcommand. An earlier --help is not proof that nothing executes.
		s.information = ""
		if enabled, err := strconv.ParseBool(option.value); err == nil && enabled {
			s.information = option.name
		}
	}
}

func (s *cloudOptionState) unresolved() {
	s.opaque = true
	s.informationUncertain = true
}

func lookupCloudOption(name string, tables []map[string]cloudOptionSpec) (cloudOptionSpec, bool) {
	for _, table := range tables {
		if spec, ok := table[name]; ok {
			return spec, true
		}
	}
	return cloudOptionSpec{}, false
}

// isCloudCommandArgv identifies only a statically parsed cloud executable behind
// known wrappers. It does not read stdin, expand shell syntax or invoke anything.
// This peek lets the normalizer retain SQL input data without passing that data
// to cloud option parsing as if it were an extra argument.
func isCloudCommandArgv(command string) bool {
	parser := shellwords.NewParser()
	parser.ParseEnv = false
	parser.ParseBacktick = false
	tokens, err := parser.Parse(command)
	if err != nil || parser.Position >= 0 {
		return false
	}
	tokens, _, valid := unwrapCommandTokens(tokens)
	return valid && len(tokens) > 0 && cloudGlobalOptions[canonicalExecutable(tokens[0])] != nil
}

// scanCloudOption consumes exactly one known flag and its value. It deliberately
// rejects unknown options, short-option bundles and abbreviations rather than
// guessing whether a later word is a value or a destructive operation.
func scanCloudOption(tokens []string, index int, tables ...map[string]cloudOptionSpec) (cloudParsedOption, bool) {
	word := tokens[index]
	name, value, equals := strings.Cut(word, "=")
	spec, ok := lookupCloudOption(name, tables)
	attached := false
	if !ok && len(word) > 2 && word[0] == '-' && word[1] != '-' {
		if short, found := lookupCloudOption(word[:2], tables); found && short.Attached {
			name, value, spec, ok, attached = word[:2], word[2:], short, true, true
			equals = false
		}
	}
	if !ok || (equals && !spec.Equals) {
		return cloudParsedOption{}, false
	}
	parsed := cloudParsedOption{name: name, next: index + 1, opaque: spec.Opaque, information: spec.Information}
	if spec.Value {
		if !equals && !attached {
			switch {
			case spec.Default != "":
				value = spec.Default
			case spec.Separate && parsed.next < len(tokens):
				value = tokens[parsed.next]
				parsed.next++
			default:
				return cloudParsedOption{}, false
			}
		}
	} else {
		if !equals {
			value = "true"
		}
		if !optionTokenIsDynamic(value) {
			if _, err := strconv.ParseBool(value); err != nil {
				return cloudParsedOption{}, false
			}
		}
	}
	parsed.value = value
	parsed.opaque = parsed.opaque || optionTokenIsDynamic(value)
	return parsed, true
}

// normalizeCloudTokens produces policy views only. Actual argv, paths, hashes
// and execution are unchanged. Configuration files are never read, and unknown
// inputs cannot borrow a SAFE match from the visible part of a command.
func normalizeCloudTokens(tokens []string) (normalized []string, opaque, valid bool) {
	if len(tokens) < 2 {
		return tokens, false, true
	}
	command := filepath.Base(tokens[0])
	options, recognized := cloudGlobalOptions[command]
	if !recognized {
		return tokens, false, true
	}
	state := &cloudOptionState{}
	for i := 1; i < len(tokens); {
		if strings.HasPrefix(tokens[i], "-") {
			option, ok := scanCloudOption(tokens, i, options)
			if !ok {
				return tokens, true, false
			}
			state.observe(option)
			i = option.next
			continue
		}
		if tokens[i] == "" || optionTokenIsDynamic(tokens[i]) {
			return tokens, true, false
		}
		result := append([]string{tokens[0]}, tokens[i:]...)
		valid = true
		switch {
		case command == "kubectl" && tokens[i] == "delete":
			result, valid = normalizeKubectlDelete(result, state)
		case command == "terraform" && (tokens[i] == "destroy" || tokens[i] == "apply"):
			result, valid = normalizeTerraformExecution(result, state)
		case command == "kubectl" || command == "helm":
			inspectCloudGlobalSuffix(result, options, state)
		}
		if valid && state.information != "" && !state.informationUncertain {
			result = []string{tokens[0], state.information}
		}
		return result, state.opaque, valid
	}
	if state.information != "" && !state.informationUncertain {
		return []string{tokens[0], state.information}, state.opaque, true
	}
	return tokens, true, false
}

// Cobra permits inherited options after ordinary subcommands and operands.
// Inspect only known global arities: an unknown subcommand-specific option may
// consume a following word, so stop conservatively rather than reinterpreting
// its data as another flag. Keep these operands in the policy view unchanged.
func inspectCloudGlobalSuffix(tokens []string, options map[string]cloudOptionSpec, state *cloudOptionState) {
	for i := 2; i < len(tokens); {
		word := tokens[i]
		if word == "--" {
			return
		}
		if !strings.HasPrefix(word, "-") {
			if optionTokenIsDynamic(word) {
				state.unresolved()
			}
			i++
			continue
		}
		option, ok := scanCloudOption(tokens, i, options)
		if !ok {
			state.unresolved()
			return
		}
		state.observe(option)
		i = option.next
	}
}

func normalizeKubectlDelete(tokens []string, state *cloudOptionState) ([]string, bool) {
	resources := make([]string, 0, len(tokens)-2)
	optionsDone := false
	for i := 2; i < len(tokens); {
		word := tokens[i]
		if !optionsDone && word == "--" {
			optionsDone = true
			i++
			continue
		}
		if !optionsDone && strings.HasPrefix(word, "-") {
			option, ok := scanCloudOption(tokens, i, kubectlDeleteOptions, cloudGlobalOptions["kubectl"])
			if !ok {
				state.unresolved()
				return tokens, false
			}
			state.observe(option)
			i = option.next
			continue
		}
		if optionTokenIsDynamic(word) {
			state.unresolved()
		}
		resources = append(resources, word)
		i++
	}
	// In TYPE NAME... form, later plain names are not resource types. Qualified
	// TYPE/NAME operands and comma-separated type lists can include multiple
	// resource kinds; a pod prefix must not hide a namespace or node alongside it.
	for i, resource := range resources {
		if i > 0 && !strings.Contains(resource, "/") {
			continue
		}
		if protected := protectedKubectlResource(resource); protected != "" {
			resources[i] = protected
			resources[0], resources[i] = resources[i], resources[0]
			break
		}
	}
	return append([]string{tokens[0], "delete"}, resources...), true
}

func protectedKubectlResource(resource string) string {
	parts := strings.Split(resource, ",")
	for i, part := range parts {
		kind, suffix, qualified := strings.Cut(part, "/")
		base, _, _ := strings.Cut(kind, ".")
		if canonical, ok := kubectlProtectedResources[base]; ok {
			parts[i] = canonical + strings.TrimPrefix(kind, base)
			if qualified {
				parts[i] += "/" + suffix
			}
			parts[0], parts[i] = parts[i], parts[0]
			return strings.Join(parts, ",")
		}
	}
	return ""
}

func normalizeTerraformExecution(tokens []string, state *cloudOptionState) ([]string, bool) {
	destroy := tokens[1] == "destroy"
	var targets []string
	valid := true
	for i := 2; i < len(tokens); {
		word := tokens[i]
		// Terraform uses Go flag parsing: a positional operand or -- stops
		// option parsing. Text inside a plan filename is not an execution flag.
		if word == "--" || !strings.HasPrefix(word, "-") {
			break
		}
		option, ok := scanCloudOption(tokens, i, terraformExecutionOptions)
		if !ok {
			state.unresolved()
			valid = false
			break
		}
		state.observe(option)
		if option.name == "-target" {
			if option.value == "" {
				state.unresolved()
				valid = false
			} else {
				targets = append(targets, "-target="+option.value)
			}
		}
		if option.name == "-destroy" && tokens[1] == "apply" {
			if value, err := strconv.ParseBool(option.value); err == nil {
				destroy = value
			} else {
				// A computed mode cannot prove that apply is non-destructive.
				state.unresolved()
			}
		}
		i = option.next
	}
	if !destroy {
		return tokens, valid
	}
	return append([]string{tokens[0], "destroy"}, targets...), valid
}
