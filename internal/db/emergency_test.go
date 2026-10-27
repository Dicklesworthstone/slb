package db

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func emergencyTestIntent(project, id string) *EmergencyExecution {
	return &EmergencyExecution{
		ID: id, ProjectPath: project,
		Command: CommandSpec{Raw: "printf emergency", Cwd: project, Shell: true},
		Actor:   "local-operator", Reason: "Recover a blocked local workflow",
		ConfirmationMethod: EmergencyConfirmationHash, LogPath: filepath.Join(project, id+".log"),
	}
}

func emergencyTestOutcome(code int) EmergencyOutcome {
	duration := int64(12)
	return EmergencyOutcome{ExitCode: &code, DurationMs: &duration}
}

func TestEmergencyExecutionLifecycleIsSeparateAndPersistent(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "state.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record := emergencyTestIntent(project, "override")
	record.RollbackPath = filepath.Join(project, "rollback")
	record.Command.DisplayRedacted = "printf [redacted]"
	record.Command.ContainsSensitive = true
	if err := database.CreateEmergencyExecution(record); err != nil {
		t.Fatal(err)
	}
	if record.Status != EmergencyStatusIncomplete || record.Assurance != AssuranceEmergencyUnreviewed ||
		record.ConfiguredApprovalPolicy != ApprovalPolicyIndependent ||
		record.RecordedAt.IsZero() || record.Command.Hash != ComputeCommandHash(record.Command) || len(record.RawCommandHash) != 64 {
		t.Fatalf("intent is incomplete or claims approval: %+v", record)
	}
	if err := database.MarkEmergencyExecutionStarted(record.ID, 4312); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteEmergencyExecution(record.ID, EmergencyStatusSucceeded, emergencyTestOutcome(0)); err != nil {
		t.Fatal(err)
	}
	before, err := database.GetEmergencyExecution(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != EmergencyStatusSucceeded || before.StartedAt == nil || before.CompletedAt == nil ||
		before.PID == nil || *before.PID != 4312 || before.ExitCode == nil || *before.ExitCode != 0 ||
		before.DurationMs == nil || before.Command.Hash != record.Command.Hash || before.Actor != record.Actor ||
		before.Reason != record.Reason || before.RollbackPath != record.RollbackPath || before.Assurance != AssuranceEmergencyUnreviewed {
		t.Fatalf("incorrect completed evidence: %+v", before)
	}
	for _, table := range []string{"sessions", "requests", "reviews", "request_events"} {
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("emergency execution created %s authority: %d %v", table, count, err)
		}
	}
	if _, err := database.VerifyRequestApproval(record.ID); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("override entered the approval workflow: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.GetEmergencyExecution(record.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("completed evidence did not survive reopen: %+v %v", after, err)
	}
}

func TestEmergencyExecutionIncompleteSurvivesReopen(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "state.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"before-spawn", "after-spawn"} {
		if err := database.CreateEmergencyExecution(emergencyTestIntent(project, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.MarkEmergencyExecutionStarted("after-spawn", 1234); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	records, err := database.ListEmergencyExecutions(EmergencyExecutionFilter{ProjectPath: project, Status: EmergencyStatusIncomplete})
	if err != nil || len(records) != 2 {
		t.Fatalf("lost incomplete executions: %+v %v", records, err)
	}
	for _, record := range records {
		if record.CompletedAt != nil || record.ExitCode != nil || record.DurationMs != nil || record.Assurance != AssuranceEmergencyUnreviewed {
			t.Fatalf("recovery invented an outcome: %+v", record)
		}
		if (record.ID == "after-spawn") != (record.StartedAt != nil && record.PID != nil && *record.PID == 1234) {
			t.Fatalf("lost startup evidence: %+v", record)
		}
	}
}

func TestEmergencyExecutionRejectsInvalidIntent(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	now := time.Now()
	pid := 123
	code := 0
	duration := int64(1)
	cases := []struct {
		name string
		edit func(*EmergencyExecution)
	}{
		{"relative project", func(r *EmergencyExecution) { r.ProjectPath = "project" }},
		{"relative cwd", func(r *EmergencyExecution) { r.Command.Cwd = "." }},
		{"empty command", func(r *EmergencyExecution) { r.Command.Raw = " \n" }},
		{"nul command", func(r *EmergencyExecution) { r.Command.Raw = "echo\x00hidden" }},
		{"empty reason", func(r *EmergencyExecution) { r.Reason = " " }},
		{"empty actor", func(r *EmergencyExecution) { r.Actor = " " }},
		{"empty log", func(r *EmergencyExecution) { r.LogPath = "" }},
		{"unknown confirmation", func(r *EmergencyExecution) { r.ConfirmationMethod = "approved" }},
		{"unknown configured policy", func(r *EmergencyExecution) { r.ConfiguredApprovalPolicy = "external-override" }},
		{"independent assurance", func(r *EmergencyExecution) { r.Assurance = AssuranceIndependent }},
		{"peer review assurance", func(r *EmergencyExecution) { r.Assurance = AssuranceSameHost }},
		{"approved status", func(r *EmergencyExecution) { r.Status = "approved" }},
		{"precompleted status", func(r *EmergencyExecution) { r.Status = EmergencyStatusSucceeded }},
		{"caller timestamp", func(r *EmergencyExecution) { r.RecordedAt = now }},
		{"caller start", func(r *EmergencyExecution) { r.StartedAt = &now }},
		{"caller completion", func(r *EmergencyExecution) { r.CompletedAt = &now }},
		{"caller pid", func(r *EmergencyExecution) { r.PID = &pid }},
		{"caller result", func(r *EmergencyExecution) { r.ExitCode = &code }},
		{"caller duration", func(r *EmergencyExecution) { r.DurationMs = &duration }},
		{"caller error", func(r *EmergencyExecution) { r.Error = "failed" }},
		{"changed command hash", func(r *EmergencyExecution) { r.Command.Hash = "incorrect" }},
		{"changed acknowledgment hash", func(r *EmergencyExecution) { r.RawCommandHash = "incorrect" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			record := emergencyTestIntent(t.TempDir(), "invalid")
			test.edit(record)
			if err := database.CreateEmergencyExecution(record); err == nil {
				t.Fatal("accepted invalid emergency intent")
			}
		})
	}
	if err := database.CreateEmergencyExecution(nil); err == nil {
		t.Fatal("accepted nil intent")
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM emergency_executions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid intents persisted: %d %v", count, err)
	}
}

func TestEmergencyExecutionOnlyObservedSuccessAndFailuresComplete(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	project := t.TempDir()
	for _, test := range []struct {
		id      string
		status  EmergencyStatus
		outcome EmergencyOutcome
		started bool
	}{
		{"never-started", EmergencyStatusFailed, EmergencyOutcome{Error: "executable unavailable"}, false},
		{"nonzero", EmergencyStatusFailed, emergencyTestOutcome(27), true},
		{"timeout", EmergencyStatusTimedOut, EmergencyOutcome{Error: "execution deadline exceeded"}, false},
		{"cancelled", EmergencyStatusCancelled, EmergencyOutcome{Error: "execution cancelled"}, true},
		{"startup-record-failed", EmergencyStatusFailed, func() EmergencyOutcome {
			outcome := emergencyTestOutcome(-1)
			outcome.Error = "startup persistence failed; child stopped"
			return outcome
		}(), false},
	} {
		t.Run(test.id, func(t *testing.T) {
			record := emergencyTestIntent(project, test.id)
			if err := database.CreateEmergencyExecution(record); err != nil {
				t.Fatal(err)
			}
			if test.started {
				if err := database.MarkEmergencyExecutionStarted(record.ID, 777); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.CompleteEmergencyExecution(record.ID, test.status, test.outcome); err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetEmergencyExecution(record.ID)
			if err != nil || stored.Status != test.status || stored.Error != test.outcome.Error ||
				!reflect.DeepEqual(stored.ExitCode, test.outcome.ExitCode) || !reflect.DeepEqual(stored.DurationMs, test.outcome.DurationMs) ||
				stored.CompletedAt == nil || (stored.StartedAt != nil) != test.started {
				t.Fatalf("wrong failure evidence: %+v %v", stored, err)
			}
		})
	}

	record := emergencyTestIntent(project, "cannot-invent-success")
	if err := database.CreateEmergencyExecution(record); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteEmergencyExecution(record.ID, EmergencyStatusSucceeded, emergencyTestOutcome(0)); !errors.Is(err, ErrEmergencyTransition) {
		t.Fatalf("success without durable startup: %v", err)
	}
	for _, test := range []struct {
		status  EmergencyStatus
		outcome EmergencyOutcome
	}{
		{EmergencyStatusSucceeded, EmergencyOutcome{}},
		{EmergencyStatusSucceeded, emergencyTestOutcome(4)},
		{EmergencyStatusSucceeded, EmergencyOutcome{ExitCode: emergencyTestOutcome(0).ExitCode, DurationMs: emergencyTestOutcome(0).DurationMs, Error: "output lost"}},
		{EmergencyStatusFailed, emergencyTestOutcome(0)},
		{EmergencyStatusTimedOut, emergencyTestOutcome(-1)},
		{EmergencyStatusCancelled, EmergencyOutcome{}},
		{EmergencyStatusIncomplete, EmergencyOutcome{}},
		{"approved", EmergencyOutcome{}},
	} {
		if err := database.CompleteEmergencyExecution(record.ID, test.status, test.outcome); err == nil {
			t.Fatalf("accepted invented completion: %s %+v", test.status, test.outcome)
		}
	}
	stored, err := database.GetEmergencyExecution(record.ID)
	if err != nil || stored.Status != EmergencyStatusIncomplete || stored.CompletedAt != nil {
		t.Fatalf("invalid transitions changed evidence: %+v %v", stored, err)
	}
}

func TestEmergencyExecutionLifecycleCompareAndSet(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "state.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	record := emergencyTestIntent(project, "once")
	if err := first.CreateEmergencyExecution(record); err != nil {
		t.Fatal(err)
	}
	if err := first.MarkEmergencyExecutionStarted(record.ID, 789); err != nil {
		t.Fatal(err)
	}
	if err := second.MarkEmergencyExecutionStarted(record.ID, 890); !errors.Is(err, ErrEmergencyTransition) {
		t.Fatalf("replaced recorded startup: %v", err)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 12)
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func(database *DB, code int) {
			defer group.Done()
			<-start
			errorsCh <- database.CompleteEmergencyExecution(record.ID, EmergencyStatusFailed, emergencyTestOutcome(code))
		}([]*DB{first, second}[i%2], i+1)
	}
	close(start)
	group.Wait()
	close(errorsCh)
	winners := 0
	for err := range errorsCh {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrEmergencyTransition) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("completion winners=%d, want 1", winners)
	}
	if err := first.MarkEmergencyExecutionStarted(record.ID, 891); !errors.Is(err, ErrEmergencyTransition) {
		t.Fatalf("reopened terminal execution: %v", err)
	}
	stored, err := first.GetEmergencyExecution(record.ID)
	if err != nil || stored.PID == nil || *stored.PID != 789 || stored.Assurance != AssuranceEmergencyUnreviewed ||
		stored.Command.Hash != record.Command.Hash || stored.Actor != record.Actor || stored.Reason != record.Reason {
		t.Fatalf("lifecycle changed immutable evidence: %+v %v", stored, err)
	}
}

func TestEmergencyExecutionWriteFailuresAndLabels(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	project := t.TempDir()
	record := emergencyTestIntent(project, "write-failure")
	journalExec(t, database, `CREATE TRIGGER emergency_create_failure BEFORE INSERT ON emergency_executions BEGIN SELECT RAISE(ABORT, 'intent unavailable'); END`)
	if err := database.CreateEmergencyExecution(record); err == nil || !record.RecordedAt.IsZero() {
		t.Fatalf("failed intent appeared committed: %+v %v", record, err)
	}
	if _, err := database.GetEmergencyExecution(record.ID); !errors.Is(err, ErrEmergencyExecutionNotFound) {
		t.Fatalf("failed intent persisted: %v", err)
	}
	journalExec(t, database, `DROP TRIGGER emergency_create_failure`)
	if err := database.CreateEmergencyExecution(record); err != nil {
		t.Fatal(err)
	}
	journalExec(t, database, `CREATE TRIGGER emergency_start_failure BEFORE UPDATE OF started_at ON emergency_executions BEGIN SELECT RAISE(ABORT, 'startup unavailable'); END`)
	if err := database.MarkEmergencyExecutionStarted(record.ID, 123); err == nil {
		t.Fatal("startup write failure ignored")
	}
	stored, err := database.GetEmergencyExecution(record.ID)
	if err != nil || stored.StartedAt != nil || stored.PID != nil || stored.Status != EmergencyStatusIncomplete {
		t.Fatalf("failed startup produced false evidence: %+v %v", stored, err)
	}
	journalExec(t, database, `CREATE TRIGGER emergency_finish_failure BEFORE UPDATE OF completed_at ON emergency_executions BEGIN SELECT RAISE(ABORT, 'outcome unavailable'); END`)
	if err := database.CompleteEmergencyExecution(record.ID, EmergencyStatusFailed, emergencyTestOutcome(7)); err == nil {
		t.Fatal("outcome write failure ignored")
	}
	stored, err = database.GetEmergencyExecution(record.ID)
	if err != nil || stored.Status != EmergencyStatusIncomplete || stored.CompletedAt != nil || stored.ExitCode != nil {
		t.Fatalf("failed completion appeared successful: %+v %v", stored, err)
	}
	journalExec(t, database, `DROP TRIGGER emergency_finish_failure`)
	if err := database.CompleteEmergencyExecution(record.ID, EmergencyStatusFailed, emergencyTestOutcome(7)); err != nil {
		t.Fatalf("could not persist observed failure after startup record failure: %v", err)
	}
	for _, statement := range []string{
		`UPDATE emergency_executions SET assurance = 'independent'`,
		`UPDATE emergency_executions SET assurance = 'same_host'`,
		`UPDATE emergency_executions SET assurance = ''`,
		`UPDATE emergency_executions SET configured_approval_policy = 'approved'`,
		`UPDATE emergency_executions SET status = 'approved'`,
	} {
		if _, err := database.Exec(statement); err == nil {
			t.Fatalf("schema allowed evidence promotion: %s", statement)
		}
	}
}

func TestEmergencyExecutionListIsScopedAndBounded(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	project := t.TempDir()
	var records []*EmergencyExecution
	for i := 0; i < 4; i++ {
		record := emergencyTestIntent(project, fmt.Sprintf("record-%d", i))
		if i == 0 {
			record.ProjectPath = t.TempDir()
		}
		if i == 2 {
			record.Actor = "SecondOperator"
			record.Reason = "Restore REDIS"
			record.ConfiguredApprovalPolicy = ApprovalPolicySameHost
		}
		if err := database.CreateEmergencyExecution(record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := database.CompleteEmergencyExecution(records[1].ID, EmergencyStatusFailed, EmergencyOutcome{Error: "startup failed"}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		filter EmergencyExecutionFilter
		ids    []string
	}{
		{EmergencyExecutionFilter{ProjectPath: project, Limit: 1}, []string{"record-3"}},
		{EmergencyExecutionFilter{ProjectPath: project, Status: EmergencyStatusIncomplete}, []string{"record-3", "record-2"}},
		{EmergencyExecutionFilter{ProjectPath: project, Actor: "SecondOperator"}, []string{"record-2"}},
		{EmergencyExecutionFilter{ProjectPath: project, ConfiguredApprovalPolicy: ApprovalPolicySameHost}, []string{"record-2"}},
		{EmergencyExecutionFilter{ProjectPath: project, ConfiguredApprovalPolicy: ApprovalPolicyIndependent}, []string{"record-3", "record-1"}},
		{EmergencyExecutionFilter{ProjectPath: project, Query: "redis"}, []string{"record-2"}},
		{EmergencyExecutionFilter{ProjectPath: project, ID: "record-1"}, []string{"record-1"}},
		{EmergencyExecutionFilter{ProjectPath: project, Since: records[1].RecordedAt, Before: records[3].RecordedAt}, []string{"record-2", "record-1"}},
		{EmergencyExecutionFilter{ProjectPath: project, ID: "record-0"}, []string{}},
		{EmergencyExecutionFilter{ProjectPath: project, Query: "' OR 1=1 --"}, []string{}},
	} {
		got, err := database.ListEmergencyExecutions(test.filter)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(got))
		for _, record := range got {
			ids = append(ids, record.ID)
		}
		if !reflect.DeepEqual(ids, test.ids) {
			t.Fatalf("filter %+v: got %v, want %v", test.filter, ids, test.ids)
		}
	}
	for _, filter := range []EmergencyExecutionFilter{
		{}, {ProjectPath: "relative"}, {ProjectPath: project, Status: "approved"},
		{ProjectPath: project, ConfiguredApprovalPolicy: "approved"},
		{ProjectPath: project, Limit: -1}, {ProjectPath: project, Limit: 10001},
		{ProjectPath: project, Since: records[3].RecordedAt, Before: records[1].RecordedAt},
	} {
		if _, err := database.ListEmergencyExecutions(filter); err == nil {
			t.Fatalf("accepted invalid or unscoped query: %+v", filter)
		}
	}
}

func TestEmergencyExecutionConfiguredPolicyIsOnlyContext(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	project := t.TempDir()
	for _, policy := range []ApprovalPolicy{ApprovalPolicyIndependent, ApprovalPolicySameHost} {
		record := emergencyTestIntent(project, "bypass-"+string(policy))
		record.ConfiguredApprovalPolicy = policy
		if err := database.CreateEmergencyExecution(record); err != nil {
			t.Fatal(err)
		}
		if err := database.MarkEmergencyExecutionStarted(record.ID, 554); err != nil {
			t.Fatal(err)
		}
		if err := database.CompleteEmergencyExecution(record.ID, EmergencyStatusSucceeded, emergencyTestOutcome(0)); err != nil {
			t.Fatal(err)
		}
		stored, err := database.GetEmergencyExecution(record.ID)
		if err != nil || stored.ConfiguredApprovalPolicy != policy || stored.Assurance != AssuranceEmergency ||
			stored.Status != EmergencyStatusSucceeded {
			t.Fatalf("bypassed configuration became stronger evidence: %+v %v", stored, err)
		}
		if _, err := database.VerifyRequestApproval(record.ID); !errors.Is(err, ErrRequestNotFound) {
			t.Fatalf("configuration context entered approval verification: %v", err)
		}
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM reviews`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("configuration context generated review evidence: %d %v", count, err)
	}
}

func TestEmergencyExecutionMigrationFromVersion7PreservesHistory(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "v7.db")
	database, err := OpenWithOptions(path, OpenOptions{CreateIfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := ensureMigrationsTable(database.conn); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version > 7 {
			continue
		}
		switch migration.Version {
		case 5:
			journalExec(t, database, `ALTER TABLE sessions ADD COLUMN session_key_digest TEXT`)
		case 6:
			// There are no session keys yet; the v6 vacuum changes no schema.
		default:
			journalExec(t, database, migration.Up)
		}
		journalExec(t, database, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, migration.Version, time.Now().UTC().Format(time.RFC3339))
	}
	requester := &Session{ProjectPath: project, AgentName: "RequestAgent", Model: "model-a"}
	reviewer := &Session{ProjectPath: project, AgentName: "ReviewAgent", Model: "model-b"}
	for _, session := range []*Session{requester, reviewer} {
		if err := database.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	request := &Request{ProjectPath: project, Command: CommandSpec{Raw: "echo reviewed", Cwd: project, Shell: true},
		RiskTier: RiskTierDangerous, ApprovalPolicy: ApprovalPolicySameHost, MinApprovals: 1,
		RequestorSessionID: requester.ID, RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		Justification: Justification{Reason: "Preserve existing peer review"}}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	if _, err := applyFixtureReview(database, request, reviewer, DecisionApprove, ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost}); err != nil {
		t.Fatal(err)
	}
	before, beforeReviews, err := database.GetRequestWithReviews(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Request{ProjectPath: project, Command: CommandSpec{Raw: "echo historical", Cwd: project, Shell: true},
		RiskTier: RiskTierDangerous, ApprovalPolicy: ApprovalPolicyIndependent, MinApprovals: 1, Status: StatusApproved,
		RequestorSessionID: requester.ID, RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		Justification: Justification{Reason: "Preserve historical local HMAC evidence"}}
	if err := database.CreateRequest(legacy); err != nil {
		t.Fatal(err)
	}
	signedAt := time.Now().UTC().Truncate(time.Second)
	legacyReview := &Review{RequestID: legacy.ID, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName,
		ReviewerModel: reviewer.Model, Decision: DecisionApprove, SignatureTimestamp: signedAt,
		Signature: ComputeReviewSignature(reviewer.SessionKey, legacy.ID, DecisionApprove, signedAt)}
	if err := database.CreateReview(legacyReview); err != nil {
		t.Fatal(err)
	}
	page := journalPage(t, database, project, "", 100)
	var tableCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'emergency_executions'`).Scan(&tableCount); err != nil || tableCount != 0 {
		t.Fatalf("fixture is not a v7 database: %d %v", tableCount, err)
	}
	if err := database.ApplyMigrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := database.ValidateSchema(); err != nil {
		t.Fatal(err)
	}
	after, afterReviews, err := database.GetRequestWithReviews(request.ID)
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeReviews, afterReviews) {
		t.Fatalf("emergency migration changed existing evidence: %v", err)
	}
	if _, err := database.VerifyRequestApproval(request.ID); err != nil {
		t.Fatalf("migration invalidated same-host proof: %v", err)
	}
	historical, err := database.GetReview(legacyReview.ID)
	if err != nil || historical.Assurance != AssuranceLegacyLocal || historical.ApprovalPolicy != ApprovalPolicyIndependent ||
		historical.SignatureVersion != 1 || !VerifyReviewEvidenceSignature(historical, reviewer.SessionKey) {
		t.Fatalf("migration changed historical approval evidence: %+v %v", historical, err)
	}
	if _, err := database.VerifyRequestApproval(legacy.ID); !errors.Is(err, ErrIndependentIdentityUnavailable) {
		t.Fatalf("emergency migration promoted historical local approval: %v", err)
	}
	if resumed := journalPage(t, database, project, page.Cursor, 100); len(resumed.Events) != 0 || resumed.Cursor != page.Cursor {
		t.Fatal("emergency migration changed existing journal identity/cursors")
	}
	if err := database.CreateEmergencyExecution(emergencyTestIntent(project, "after-upgrade")); err != nil {
		t.Fatal(err)
	}
	if resumed := journalPage(t, database, project, page.Cursor, 100); len(resumed.Events) != 0 || resumed.Cursor != page.Cursor {
		t.Fatal("emergency intent was promoted into reviewed request history")
	}
	if err := database.ApplyMigrations(context.Background()); err != nil {
		t.Fatalf("emergency migration was not idempotent: %v", err)
	}
}
