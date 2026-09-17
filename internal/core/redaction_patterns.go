package core

// RedactionPatterns returns an independent copy of the default display rules.
// Generated offline hooks use this same list rather than maintaining a second
// privacy policy in Python. Custom per-request redactions are not global rules.
func RedactionPatterns() []string {
	return append([]string(nil), defaultRedactionPatterns...)
}
