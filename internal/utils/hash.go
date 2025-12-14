// Package utils provides utility functions for SLB.
package utils

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// CommandHash computes the canonical hash for a command.
// Hash = sha256(raw + cwd + argv_json + shell)
// This binds approvals to the exact command context.
func CommandHash(raw, cwd, shell string, argv []string) string {
	h := sha256.New()
	h.Write([]byte(raw))
	h.Write([]byte(cwd))
	argvJSON, _ := json.Marshal(argv)
	h.Write(argvJSON)
	h.Write([]byte(shell))
	return hex.EncodeToString(h.Sum(nil))
}

// HMAC computes an HMAC-SHA256 signature.
func HMAC(key, message []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(message)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyHMAC verifies an HMAC-SHA256 signature.
func VerifyHMAC(key, message []byte, signature string) bool {
	expected := HMAC(key, message)
	return hmac.Equal([]byte(expected), []byte(signature))
}
