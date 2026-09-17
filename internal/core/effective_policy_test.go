package core

import (
	"os/exec"
	"sync"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/config"
)

func TestEffectivePolicyClassificationAndExport(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Patterns.Dangerous.MinApprovals = 3
	cfg.Patterns.Critical.MinApprovals = 5
	cfg.Patterns.Dangerous.Patterns = append(cfg.Patterns.Dangerous.Patterns, `^policy-destroy$`)
	engine := NewPatternEngine()
	if _, err := engine.ReplacePolicy(cfg, nil); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"policy-destroy", "echo ready && policy-destroy", "git reset --hard HEAD"} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if got.Tier != RiskTierDangerous || got.MinApprovals != 3 || !got.NeedsApproval {
				t.Fatalf("configured policy ignored: %+v", got)
			}
		})
	}
	if got := engine.ClassifyCommand("rm -rf / trailing.log", ""); got.IsSafe || got.MinApprovals != 5 {
		t.Fatalf("configuration reintroduced weak SAFE rule: %+v", got)
	}
	if got := engine.Export().Tiers["dangerous"].MinApprovals; got != 3 {
		t.Fatalf("export lost quorum: %d", got)
	}
	before := engine.ComputeHash()
	cfg.Patterns.Dangerous.MinApprovals = 4
	if _, err := engine.ReplacePolicy(cfg, nil); err != nil {
		t.Fatal(err)
	}
	if engine.ComputeHash() == before {
		t.Fatal("approval-only change did not change policy hash")
	}
}

func TestEffectivePolicyReplacesArraysAndCustomRules(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Patterns.Safe.Patterns = []string{`^local-allow$`}
	engine := NewPatternEngine()
	custom := []Pattern{{Tier: RiskTierDangerous, Pattern: `^custom-risk$`, Source: "agent"}}
	if count, err := engine.ReplacePolicy(cfg, custom); err != nil || count != 1 {
		t.Fatalf("compile policy: count=%d err=%v", count, err)
	}
	if !engine.ClassifyCommand("local-allow", "").IsSafe || engine.ClassifyCommand("git stash", "").IsSafe {
		t.Fatal("configured SAFE array did not replace defaults")
	}
	if !engine.ClassifyCommand("custom-risk", "").NeedsApproval {
		t.Fatal("persisted restrictive pattern not merged")
	}
	if _, err := engine.ReplacePolicy(config.DefaultConfig(), nil); err != nil {
		t.Fatal(err)
	}
	if engine.ClassifyCommand("local-allow", "").IsSafe || engine.ClassifyCommand("custom-risk", "").NeedsApproval {
		t.Fatal("stale rules survived a project-policy reload")
	}
}

func TestEffectivePolicyDynamicFloorAndModelRequirement(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Patterns.Critical.MinApprovals = 5
	cfg.Patterns.Critical.DynamicQuorum = true
	cfg.Patterns.Critical.DynamicQuorumFloor = 2
	cfg.General.RequireDifferentModel = true
	engine := NewPatternEngine()
	if _, err := engine.ReplacePolicy(cfg, nil); err != nil {
		t.Fatal(err)
	}
	for available, want := range map[int]int{-1: 5, 0: 2, 1: 2, 2: 2, 3: 3, 8: 5} {
		if got := engine.RequiredApprovals(RiskTierCritical, available); got != want {
			t.Errorf("available=%d: got %d, want %d", available, got, want)
		}
	}
	if !engine.RequiresDifferentModel() || engine.RequiredApprovals(RiskTierCaution, 0) != 1 {
		t.Fatal("different-model requirement permitted zero reviews")
	}
}

func TestEffectivePolicyRejectsInvalidReplacement(t *testing.T) {
	cases := map[string]func(*config.Config){
		"legacy-unsafe-default": func(c *config.Config) { c.Patterns.Safe.Patterns = []string{`^rm\s+.*\.log$`} },
		"invalid-regex":         func(c *config.Config) { c.Patterns.Safe.Patterns = []string{`[`} },
		"zero-quorum":           func(c *config.Config) { c.Patterns.Dangerous.MinApprovals = 0 },
		"zero-floor": func(c *config.Config) {
			c.Patterns.Critical.DynamicQuorum = true
			c.Patterns.Critical.DynamicQuorumFloor = 0
		},
		"excessive-floor": func(c *config.Config) {
			c.Patterns.Critical.DynamicQuorum = true
			c.Patterns.Critical.DynamicQuorumFloor = 9
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			engine := NewPatternEngine()
			before := engine.ComputeHash()
			cfg := config.DefaultConfig()
			mutate(&cfg)
			if _, err := engine.ReplacePolicy(cfg, nil); err == nil {
				t.Fatal("invalid policy accepted")
			}
			if engine.ComputeHash() != before {
				t.Fatal("failed compile published partial policy")
			}
		})
	}
}

func TestEffectivePolicyDefaultsDoNotAlias(t *testing.T) {
	first := config.DefaultConfig()
	original := first.Patterns.Safe.Patterns[0]
	first.Patterns.Safe.Patterns[0] = `.*`
	if config.DefaultConfig().Patterns.Safe.Patterns[0] != original {
		t.Fatal("a project mutated process-wide default allow rules")
	}
}

func TestEffectivePolicyConcurrentThresholdPublication(t *testing.T) {
	engine := NewPatternEngine()
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				r := engine.ClassifyCommand("git reset --hard HEAD", "")
				if r.Tier != RiskTierDangerous || (r.MinApprovals != 1 && r.MinApprovals != 4) {
					t.Errorf("mixed policy snapshot: %+v", r)
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		cfg := config.DefaultConfig()
		if i%2 == 0 {
			cfg.Patterns.Dangerous.MinApprovals = 4
		}
		if _, err := engine.ReplacePolicy(cfg, nil); err != nil {
			t.Error(err)
		}
	}
	readers.Wait()
}

func TestEffectivePolicyPythonExportUsesConfiguredThresholds(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	cfg := config.DefaultConfig()
	cfg.Patterns.Dangerous.MinApprovals = 4
	engine := NewPatternEngine()
	if _, err := engine.ReplacePolicy(cfg, nil); err != nil {
		t.Fatal(err)
	}
	script := engine.ExportClaudeHook() + `
assert classify('git reset --hard HEAD') == ('dangerous', 4)
assert classify('rm -rf / trailing.log')[0] == 'critical'
assert classify('chmod 700 /etc/test')[0] == 'critical'
assert classify('rm test.log') == ('safe', 0)
`
	if output, err := exec.Command(python, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("Python export failed: %v\n%s", err, output)
	}
}
