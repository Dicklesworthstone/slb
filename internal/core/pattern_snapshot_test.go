package core

import (
	"sync"
	"testing"
)

func TestReplaceCustomPatternsRemovesStaleAllowRules(t *testing.T) {
	engine := NewPatternEngine()
	builtinHash := engine.ComputeHash()
	for _, tier := range []RiskTier{RiskTier(RiskSafe), RiskTierDangerous} {
		rule := Pattern{Tier: tier, Pattern: `^snapshot-test-command$`, Source: "human"}
		for i := 0; i < 2; i++ {
			count, err := engine.ReplaceCustomPatterns([]Pattern{rule, rule})
			if err != nil || count != 1 {
				t.Fatalf("reload %d: count=%d error=%v", i, count, err)
			}
			matches := 0
			for _, p := range engine.ListPatterns(tier) {
				if p.Pattern == rule.Pattern {
					matches++
				}
			}
			if matches != 1 {
				t.Fatalf("duplicate rule copies: %d", matches)
			}
		}
		if _, err := engine.ReplaceCustomPatterns(nil); err != nil {
			t.Fatal(err)
		}
		if engine.ComputeHash() != builtinHash {
			t.Fatalf("stale %s rule survived replacement", tier)
		}
	}
}

func TestReplaceCustomPatternsRejectsInvalidSnapshotAtomically(t *testing.T) {
	for _, invalid := range []Pattern{
		{Tier: RiskTier("typo"), Pattern: `.*`},
		{Tier: RiskTier(""), Pattern: `.*`},
		{Tier: RiskTierDangerous, Pattern: `[`},
		{Tier: RiskTier(RiskSafe), Pattern: `[`},
	} {
		t.Run(string(invalid.Tier)+invalid.Pattern, func(t *testing.T) {
			engine := NewPatternEngine()
			before := engine.ComputeHash()
			_, err := engine.ReplaceCustomPatterns([]Pattern{
				{Tier: RiskTier(RiskSafe), Pattern: `.*`}, invalid,
			})
			if err == nil {
				t.Fatal("invalid policy was accepted")
			}
			if engine.ComputeHash() != before {
				t.Fatal("failed reload published a partial allowlist")
			}
		})
	}
}

func TestReplaceCustomPatternsPublishesWholeSnapshots(t *testing.T) {
	engine := NewPatternEngine()
	baseline := engine.ComputeHash()
	rules := []Pattern{{Tier: RiskTier(RiskSafe), Pattern: `^snapshot-reader$`},
		{Tier: RiskTierCritical, Pattern: `^snapshot-writer$`}}
	if _, err := engine.ReplaceCustomPatterns(rules); err != nil {
		t.Fatal(err)
	}
	custom := engine.ComputeHash()
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				hash := engine.ComputeHash()
				if hash != baseline && hash != custom {
					t.Errorf("partial policy snapshot: %s", hash)
					return
				}
			}
		}()
	}
	for i := 0; i < 30; i++ {
		candidate := rules
		if i%2 == 0 {
			candidate = nil
		}
		if _, err := engine.ReplaceCustomPatterns(candidate); err != nil {
			t.Error(err)
		}
	}
	readers.Wait()
}
