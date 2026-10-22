package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// hookPolicyProject creates an initialized project with a state database, as
// `slb init` would, and a private HOME so the host's user config is not read.
func hookPolicyProject(tb testing.TB) string {
	tb.Helper()
	tb.Setenv("HOME", tb.TempDir())
	tb.Setenv("USERPROFILE", tb.TempDir())
	project, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		tb.Fatal(err)
	}
	if err := database.Close(); err != nil {
		tb.Fatal(err)
	}
	return project
}

// ageStateFiles moves the state database's timestamps out of the racy window,
// as if the last write happened an hour ago, so the cache trusts its stat.
func ageStateFiles(t *testing.T, project string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	for _, path := range []string{"state.db", "state.db-wal"} {
		err := os.Chtimes(filepath.Join(project, ".slb", path), old, old)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func openProjectDB(t *testing.T, project string) *db.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestHookPolicyCacheReusesCompiledPolicy(t *testing.T) {
	project := hookPolicyProject(t)
	ageStateFiles(t, project)
	var cache hookPolicyCache
	first, err := cache.policyFor(project)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.policyFor(project)
	if err != nil {
		t.Fatal(err)
	}
	if first.engine != second.engine {
		t.Fatal("an unchanged project recompiled its hook policy")
	}
	if second.statePath != filepath.Join(project, ".slb", "state.db") || second.cautionAction != "block" {
		t.Fatalf("unexpected cached policy: %+v", second)
	}
}

func TestHookPolicyCacheSeesCustomPatternAfterQuietPeriod(t *testing.T) {
	project := hookPolicyProject(t)
	conn := openProjectDB(t, project)
	ageStateFiles(t, project)
	srv := &IPCServer{}
	params := HookQueryParams{Command: "example-cleanup", CWD: project}
	if result := srv.classifyCommand(params); result.Action != "allow" {
		t.Fatalf("unexpected baseline: %+v", result)
	}
	if _, err := conn.InsertCustomPattern("critical", "^example-cleanup$", "project policy", "human"); err != nil {
		t.Fatal(err)
	}
	if result := srv.classifyCommand(params); result.Action != "block" || result.Tier != "critical" {
		t.Fatalf("custom rule added after a quiet period was not applied: %+v", result)
	}
}

func TestHookPolicyCacheKeepsEngineWhenOnlyOtherStateChanges(t *testing.T) {
	project := hookPolicyProject(t)
	conn := openProjectDB(t, project)
	ageStateFiles(t, project)
	var cache hookPolicyCache
	before, err := cache.policyFor(project)
	if err != nil {
		t.Fatal(err)
	}
	quiet := cache.entries[project].state
	if quiet.racy {
		t.Fatal("aged state files were treated as racy")
	}
	if err := conn.CreateSession(&db.Session{AgentName: "CacheAgent", Model: "test", Program: "test", ProjectPath: project}); err != nil {
		t.Fatal(err)
	}
	after, err := cache.policyFor(project)
	if err != nil {
		t.Fatal(err)
	}
	if cache.entries[project].state.unchangedSince(quiet) {
		t.Fatal("the session write did not change the state stamp; the re-read path was not exercised")
	}
	if before.engine != after.engine {
		t.Fatal("a write that left custom_patterns unchanged recompiled the policy")
	}
}

func TestHookPolicyCacheReloadsConfigAndEnvironment(t *testing.T) {
	project := hookPolicyProject(t)
	ageStateFiles(t, project)
	srv := &IPCServer{}
	params := HookQueryParams{Command: "user-policy-risk", CWD: project}
	if result := srv.classifyCommand(params); result.Action != "allow" {
		t.Fatalf("unexpected baseline: %+v", result)
	}

	userConfig := filepath.Join(os.Getenv("HOME"), ".slb", "config.toml")
	if err := os.MkdirAll(filepath.Dir(userConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfig, []byte("[patterns.critical\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := srv.classifyCommand(params); result.Action != "block" || result.MatchedPattern != "policy_load_error" {
		t.Fatalf("broken user config failed open: %+v", result)
	}
	if err := os.WriteFile(userConfig, []byte("[patterns.critical]\npatterns = ['^user-policy-risk$']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := srv.classifyCommand(params); result.Action != "block" || result.Tier != "critical" {
		t.Fatalf("user config edit was not applied: %+v", result)
	}

	caution := HookQueryParams{Command: "rm notes.txt", CWD: project}
	if result := srv.classifyCommand(caution); result.Action != "block" || result.Tier != "caution" {
		t.Fatalf("unexpected caution baseline: %+v", result)
	}
	t.Setenv("SLB_HOOK_CAUTION_ACTION", "ask")
	if result := srv.classifyCommand(caution); result.Action != "ask" {
		t.Fatalf("SLB_HOOK_CAUTION_ACTION change was not applied: %+v", result)
	}
}

func TestHookPolicyCacheUntrustsRecentStateWrites(t *testing.T) {
	project := hookPolicyProject(t)
	path := filepath.Join(project, ".slb", "state.db")
	fresh, err := statState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.racy {
		t.Fatal("a state database written moments ago was trusted")
	}
	same, err := statState(path)
	if err != nil {
		t.Fatal(err)
	}
	if same.unchangedSince(fresh) {
		t.Fatal("an identical stat after a racy one was treated as proof of no change")
	}
	ageStateFiles(t, project)
	quiet, err := statState(path)
	if err != nil {
		t.Fatal(err)
	}
	if quiet.racy {
		t.Fatal("an hour-old state database was treated as racy")
	}
	again, err := statState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.unchangedSince(quiet) {
		t.Fatal("two identical quiet stats were treated as a change")
	}
}

func TestHookPolicyCacheIsBounded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var cache hookPolicyCache
	for i := 0; i < maxCachedHookPolicies+8; i++ {
		if _, err := cache.policyFor(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(cache.entries); got > maxCachedHookPolicies {
		t.Fatalf("cache holds %d entries, bound is %d", got, maxCachedHookPolicies)
	}
}

func TestHookPolicyFollowsPhysicalProjectThroughAliases(t *testing.T) {
	project := hookPolicyProject(t)
	other := hookPolicyProject(t)
	physical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(physical, "src")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(project, ".slb", "config.toml")
	if err := os.WriteFile(configPath, []byte("[patterns.critical]\npatterns = ['^alias-config-wipe$']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openProjectDB(t, project).InsertCustomPattern("critical", "^alias-db-wipe$", "project A rule", "human"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".slb", "config.toml"),
		[]byte("[patterns.safe]\npatterns = ['^alias-config-wipe$', '^alias-db-wipe$']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootAlias := filepath.Join(t.TempDir(), "root-alias")
	externalAlias := filepath.Join(t.TempDir(), "descendant-alias")
	otherAlias := filepath.Join(other, "descendant-alias")
	for alias, target := range map[string]string{rootAlias: project, externalAlias: nested, otherAlias: nested} {
		if err := os.Symlink(target, alias); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
	}
	srv := &IPCServer{}
	for _, command := range []string{"alias-config-wipe", "alias-db-wipe"} {
		if result := srv.classifyCommand(HookQueryParams{Command: command, CWD: other}); result.Action != "allow" {
			t.Fatalf("project B control did not use its safe policy: %+v", result)
		}
		for _, tc := range []struct{ name, cwd string }{
			{"physical", nested}, {"root alias", rootAlias},
			{"external descendant alias", externalAlias}, {"alias below project B", otherAlias},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				if SocketPathForCWD(tc.cwd) != SocketPathForCWD(physical) {
					t.Fatal("fixture did not route to project A's daemon")
				}
				result := srv.classifyCommand(HookQueryParams{Command: command, CWD: tc.cwd})
				if result.Action != "block" || result.Tier != "critical" {
					t.Fatalf("project A's policy was bypassed through %q: %+v", tc.cwd, result)
				}
			})
		}
	}
	if err := os.WriteFile(configPath, []byte("[patterns.critical\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{nested, externalAlias, otherAlias} {
		result := srv.classifyCommand(HookQueryParams{Command: "alias-config-wipe", CWD: cwd})
		if result.Action != "block" || result.MatchedPattern != "policy_load_error" {
			t.Errorf("unavailable project A policy did not fail closed through %q: %+v", cwd, result)
		}
	}
}

// BenchmarkHookQueryClassify measures the steady state: a project whose state
// database has not been written in the last few seconds.
func BenchmarkHookQueryClassify(b *testing.B) {
	project := hookPolicyProject(b)
	srv := &IPCServer{}
	params := HookQueryParams{Command: "cat README.md", SessionID: "bench-session", CWD: project}
	// The first query's read-only open may leave an empty WAL behind; age
	// every state file afterwards so the measured queries see a quiet project.
	srv.classifyCommand(params)
	old := time.Now().Add(-time.Hour)
	for _, name := range []string{"state.db", "state.db-wal"} {
		if err := os.Chtimes(filepath.Join(project, ".slb", name), old, old); err != nil && !os.IsNotExist(err) {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := srv.classifyCommand(params); got.Action != "allow" {
			b.Fatalf("unexpected result %+v", got)
		}
	}
}
