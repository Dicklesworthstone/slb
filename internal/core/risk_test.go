package core

import (
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestClassifyRisk(t *testing.T) {
	// ClassifyRisk currently returns dangerous as default
	tests := []struct {
		name    string
		command string
		want    RiskTier
	}{
		{"any command returns dangerous", "rm -rf /", RiskTierDangerous},
		{"empty command returns dangerous", "", RiskTierDangerous},
		{"simple command returns dangerous", "ls -la", RiskTierDangerous},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyRisk(tt.command); got != tt.want {
				t.Errorf("ClassifyRisk(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestMinApprovalsForTier(t *testing.T) {
	tests := []struct {
		name string
		tier RiskTier
		want int
	}{
		{"critical", RiskTierCritical, 2},
		{"dangerous", RiskTierDangerous, 1},
		{"caution", RiskTierCaution, 0},
		{"safe", RiskTier(RiskSafe), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MinApprovalsForTier(tt.tier); got != tt.want {
				t.Errorf("MinApprovalsForTier(%q) = %d, want %d", tt.tier, got, tt.want)
			}
		})
	}
}

func TestIsSafeTier(t *testing.T) {
	tests := []struct {
		tier RiskTier
		want bool
	}{
		{RiskTier(RiskSafe), true},
		{RiskTierCritical, false},
		{RiskTierDangerous, false},
		{RiskTierCaution, false},
	}

	for _, tt := range tests {
		if got := IsSafeTier(tt.tier); got != tt.want {
			t.Errorf("IsSafeTier(%q) = %v, want %v", tt.tier, got, tt.want)
		}
	}
}

func TestTypeAliases(t *testing.T) {
	// Verify type aliases work correctly
	// The explicit types are what this test checks.
	var tier RiskTier = db.RiskTierCritical //nolint:staticcheck // QF1011
	if tier != RiskTierCritical {
		t.Errorf("RiskTier alias mismatch")
	}

	var status RequestStatus = db.StatusPending //nolint:staticcheck // QF1011
	if status != StatusPending {
		t.Errorf("RequestStatus alias mismatch")
	}

	var decision Decision = db.DecisionApprove //nolint:staticcheck // QF1011
	if decision != DecisionApprove {
		t.Errorf("Decision alias mismatch")
	}
}

func TestConstantReexports(t *testing.T) {
	// Verify constants are correctly re-exported
	tests := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		// Risk tiers
		{"RiskTierCritical", RiskTierCritical, db.RiskTierCritical},
		{"RiskTierDangerous", RiskTierDangerous, db.RiskTierDangerous},
		{"RiskTierCaution", RiskTierCaution, db.RiskTierCaution},

		// Statuses
		{"StatusPending", StatusPending, db.StatusPending},
		{"StatusApproved", StatusApproved, db.StatusApproved},
		{"StatusRejected", StatusRejected, db.StatusRejected},
		{"StatusExecuting", StatusExecuting, db.StatusExecuting},
		{"StatusExecuted", StatusExecuted, db.StatusExecuted},
		{"StatusExecutionFailed", StatusExecutionFailed, db.StatusExecutionFailed},
		{"StatusCancelled", StatusCancelled, db.StatusCancelled},
		{"StatusTimeout", StatusTimeout, db.StatusTimeout},
		{"StatusTimedOut", StatusTimedOut, db.StatusTimedOut},
		{"StatusEscalated", StatusEscalated, db.StatusEscalated},

		// Decisions
		{"DecisionApprove", DecisionApprove, db.DecisionApprove},
		{"DecisionReject", DecisionReject, db.DecisionReject},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestRiskSafeConstant(t *testing.T) {
	if RiskSafe != "safe" {
		t.Errorf("RiskSafe = %q, want %q", RiskSafe, "safe")
	}
}
