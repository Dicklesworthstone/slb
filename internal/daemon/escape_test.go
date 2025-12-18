package daemon

import (
	"strings"
	"testing"
)

func TestEscapeAppleScript_Vulnerability(t *testing.T) {
	// Attack payload: breaks out of string and executes command
	// In AppleScript: display notification "foo"
	//                 do shell script "echo pwned"
	// The \r acts as a statement terminator in some contexts or just breaks the string literal
	input := "foo\"" + "\r" + "do shell script \"echo pwned"
	
escaped := escapeAppleScript(input)
	
	// If \r is not escaped, it persists in the output
	if strings.Contains(escaped, "\r") {
		t.Errorf("Vulnerability found: CR not escaped. Output: %q", escaped)
	}
	
	// We verify that other escapes are working
	if !strings.Contains(escaped, "\\\"") {
		t.Errorf("Expected quotes to be escaped, got: %q", escaped)
	}
}
