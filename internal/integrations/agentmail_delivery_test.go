package integrations

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestJournalNotificationsSuppressLegacyCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	// An explicit configuration has selected journal mode, overriding this
	// project file. The transport must not independently reload a different policy.
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte("[notifications.requests]\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "legacy-called")
	t.Setenv("SLB_TEST_MAIL_MARKER", marker)
	if err := os.WriteFile(filepath.Join(bin, "mcp-agent-mail"), []byte("#!/bin/sh\nprintf called > \"$SLB_TEST_MAIL_MARKER\"\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	client := NewAgentMailClient(project, "", "", true)
	request := &db.Request{ID: "req-test", RiskTier: db.RiskTierDangerous, Command: db.CommandSpec{Raw: "API_KEY=secret", ContainsSensitive: true}}
	review := &db.Review{}
	for _, notify := range []func() error{
		func() error { return client.NotifyNewRequest(request) },
		func() error { return client.NotifyRequestApproved(request, review) },
		func() error { return client.NotifyRequestRejected(request, review) },
		func() error { return client.NotifyRequestExecuted(request, nil, 0) },
	} {
		if err := notify(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("journal mode called legacy transport")
	}
	// Once the caller has loaded the policy, a malformed unrelated project file
	// must not replace that decision or make journal mode fall back to a send.
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte("invalid TOML ["), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewAgentMailClient(project, "", "", true).send("subject", "body", ImportanceNormal); err != nil {
		t.Fatalf("transport reloaded unrelated project configuration: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("journal mode fell back to legacy transport")
	}
}

func TestLegacySensitiveCommandNeverFallsBackToRaw(t *testing.T) {
	request := &db.Request{Command: db.CommandSpec{Raw: "password=must-not-leak", ContainsSensitive: true}}
	if strings.Contains(safeDisplay(request), "must-not-leak") {
		t.Fatal("sensitive fallback leaked")
	}
	request.Command.DisplayRedacted = "password=[REDACTED]"
	if safeDisplay(request) != request.Command.DisplayRedacted {
		t.Fatal("lost redacted display")
	}
}

func TestReviewNotificationReportsCommittedStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	t.Setenv("HOME", t.TempDir())
	bin, output := t.TempDir(), t.TempDir()
	bodyPath := filepath.Join(output, "body")
	t.Setenv("SLB_TEST_MAIL_BODY", bodyPath)
	script := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = '--body-md' ]; then\n    shift\n    printf '%s' \"$1\" > \"$SLB_TEST_MAIL_BODY\"\n    exit 0\n  fi\n  shift\ndone\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "mcp-agent-mail"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	client := NewAgentMailClient(t.TempDir(), "fixture", "fixture", false)
	for _, tc := range []struct {
		decision db.Decision
		status   db.RequestStatus
	}{
		{db.DecisionApprove, db.StatusPending},
		{db.DecisionApprove, db.StatusApproved},
		{db.DecisionReject, db.StatusApproved},
		{db.DecisionReject, db.StatusRejected},
		{db.DecisionReject, db.StatusEscalated},
	} {
		t.Run(string(tc.decision)+"/"+string(tc.status), func(t *testing.T) {
			request := &db.Request{ID: "review-fixture", Status: tc.status, ApprovalPolicy: db.ApprovalPolicySameHost,
				Command: db.CommandSpec{Raw: "secret=must-not-leak", ContainsSensitive: true}}
			review := &db.Review{Decision: tc.decision, ReviewerAgent: "peer", Assurance: db.AssuranceSameHost}
			var err error
			prefix := "Rejection recorded for request "
			if tc.decision == db.DecisionApprove {
				prefix = "Approval recorded for request "
				err = client.NotifyRequestApproved(request, review)
			} else {
				err = client.NotifyRequestRejected(request, review)
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(bodyPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{prefix + request.ID, "Request status: " + string(tc.status) + ".\n",
				"Review policy: " + db.ApprovalPolicySameHost.Label(), "Evidence: same_host."} {
				if !strings.Contains(string(body), expected) {
					t.Fatalf("review notice lost %q: %s", expected, body)
				}
			}
			if strings.Contains(string(body), "must-not-leak") {
				t.Fatal("review notification leaked a sensitive command")
			}
		})
	}
}

func TestExecutionNotificationPreservesReceiptProvenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	bin := t.TempDir()
	bodyPath := filepath.Join(t.TempDir(), "message-body")
	t.Setenv("SLB_TEST_MAIL_BODY", bodyPath)
	script := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = '--body-md' ]; then\n    shift\n    printf '%s' \"$1\" > \"$SLB_TEST_MAIL_BODY\"\n    exit 0\n  fi\n  shift\ndone\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "mcp-agent-mail"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	client := NewAgentMailClient(t.TempDir(), "fixture", "fixture", false)
	for _, tc := range []struct {
		name          string
		requestPolicy db.ApprovalPolicy
		receipt       *db.Execution
		wantPolicy    db.ApprovalPolicy
		wantEvidence  string
	}{
		{"historical receipt", db.ApprovalPolicySameHost, &db.Execution{ApprovalPolicy: db.ApprovalPolicyIndependent, Assurance: db.AssuranceLegacyUnknown}, db.ApprovalPolicyIndependent, "legacy_unknown"},
		{"same-host receipt", db.ApprovalPolicyIndependent, &db.Execution{ApprovalPolicy: db.ApprovalPolicySameHost, Assurance: db.AssuranceSameHost}, db.ApprovalPolicySameHost, "same_host"},
		{"missing receipt", db.ApprovalPolicySameHost, nil, db.ApprovalPolicySameHost, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := &db.Request{ID: "receipt-fixture", ApprovalPolicy: tc.requestPolicy, Command: db.CommandSpec{Raw: "echo fixture"}}
			if err := client.NotifyRequestExecuted(request, tc.receipt, 0); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(bodyPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "Review policy: "+tc.wantPolicy.Label()+".\n") {
				t.Fatalf("execution notice lost receipt policy: %s", body)
			}
			if tc.wantEvidence == "" {
				if strings.Contains(string(body), "Execution evidence:") {
					t.Fatalf("missing receipt invented execution assurance: %s", body)
				}
			} else if !strings.Contains(string(body), "Execution evidence: "+tc.wantEvidence+".\n") {
				t.Fatalf("execution notice relabeled receipt assurance: %s", body)
			}
		})
	}
}
