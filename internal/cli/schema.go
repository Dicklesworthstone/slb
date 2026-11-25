package cli

import (
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/background"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	slbgit "github.com/Dicklesworthstone/slb/internal/git"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagSchema    bool
	flagSchemaAll bool
)

type schemaEntry struct {
	Key         string
	Slug        string
	Title       string
	Command     string
	Description string
	Aliases     []string
	Streaming   bool
	Formats     []string
	Build       func() *output.JSONSchema
}

var (
	standardFormats = []string{"json", "yaml", "toon"}
	ndjsonFormats   = []string{"ndjson", "json"}
)

var (
	approvalPolicyValues  = []string{string(db.ApprovalPolicyIndependent), string(db.ApprovalPolicySameHost)}
	reviewAssuranceValues = []string{
		string(db.AssuranceIndependent),
		string(db.AssuranceSameHost),
		string(db.AssuranceLegacyLocal),
		string(db.AssuranceLegacyUnknown),
		string(db.AssurancePolicyAuto),
		string(db.AssuranceEmergency),
	}
	riskTierValues = []string{
		string(core.RiskSafe),
		string(db.RiskTierCaution),
		string(db.RiskTierDangerous),
		string(db.RiskTierCritical),
	}
	requestStatusValues = []string{
		string(db.StatusPending),
		string(db.StatusApproved),
		string(db.StatusRejected),
		string(db.StatusExecuting),
		string(db.StatusExecuted),
		string(db.StatusExecutionFailed),
		string(db.StatusTimedOut),
		string(db.StatusCancelled),
		string(db.StatusTimeout),
		string(db.StatusEscalated),
	}
)

var schemaCmd = &cobra.Command{
	Use:   "schema [command...]",
	Short: "Emit JSON Schema (Draft 2020-12) for machine-readable command output",
	Long: `Emit JSON Schema (Draft 2020-12) documents describing SLB's structured command outputs.

Agents can use these schemas to validate responses programmatically, inspect field
descriptions, or generate typed clients.

Examples:
  slb schema                     # List all commands with schemas
  slb schema pending             # Emit JSON Schema for 'slb pending --json'
  slb schema config              # Emit JSON Schema for 'slb config --json'
  slb schema request             # Emit JSON Schema for 'slb request --json'
  slb schema history             # Emit JSON Schema for 'slb history --json'
  slb schema daemon status       # Emit JSON Schema for 'slb daemon status --json'
  slb schema --all               # Emit a bundle of all schemas
  slb pending --schema           # Equivalent shorthand via global --schema flag`,
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagSchemaAll {
			return writeSchemaValue(cmd.OutOrStdout(), buildAllSchemasBundle())
		}
		if len(args) == 0 || (len(args) == 1 && strings.EqualFold(args[0], "list")) {
			if schemaFlagEnabled(cmd) {
				schema, err := LookupCommandSchema("schema")
				if err != nil {
					return err
				}
				return writeSchemaValue(cmd.OutOrStdout(), schema)
			}
			return writeSchemaCatalog(cmd.OutOrStdout())
		}
		key := strings.Join(args, " ")
		schema, err := LookupCommandSchema(key)
		if err != nil {
			return err
		}
		return writeSchemaValue(cmd.OutOrStdout(), schema)
	},
}

func init() {
	schemaCmd.Flags().BoolVar(&flagSchemaAll, "all", false, "emit a JSON object containing all command schemas")
	rootCmd.AddCommand(schemaCmd)
}

func writeSchemaValue(w io.Writer, value any) error {
	format, err := schemaOutputFormat()
	if err != nil {
		return err
	}
	out := output.New(format, output.WithOutput(w), output.WithStats(GetStats()))
	return out.Write(value)
}

// schemaOutputFormat defaults to JSON so bare `slb schema <cmd>` and
// `slb <cmd> --schema` emit valid JSON Schema on stdout, while honoring explicit
// `-o yaml`, `-o toon`, `--toon`, or `--json` overrides and rejecting invalid
// `-o` format names.
func schemaOutputFormat() (output.Format, error) {
	raw := strings.ToLower(strings.TrimSpace(flagOutput))
	switch raw {
	case "", "text", "json", "yaml", "toon":
	default:
		return "", fmt.Errorf("unsupported format: %s (use json, yaml, or toon)", flagOutput)
	}
	if flagJSON {
		return output.FormatJSON, nil
	}
	if flagTOON {
		return output.FormatTOON, nil
	}
	switch raw {
	case "yaml":
		return output.FormatYAML, nil
	case "toon":
		return output.FormatTOON, nil
	default:
		return output.FormatJSON, nil
	}
}

type schemaCatalogItem struct {
	Command     string   `json:"command"`
	Key         string   `json:"key"`
	ID          string   `json:"$id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Aliases     []string `json:"aliases,omitempty"`
	Streaming   bool     `json:"streaming,omitempty"`
	Formats     []string `json:"formats"`
}

func buildSchemaCatalog() map[string]any {
	entries := registeredSchemas()
	items := make([]schemaCatalogItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, schemaCatalogItem{
			Command:     e.Command,
			Key:         e.Key,
			ID:          output.SchemaIDPrefix + e.Slug + ".schema.json",
			Title:       e.Title,
			Description: e.Description,
			Aliases:     e.Aliases,
			Streaming:   e.Streaming,
			Formats:     e.Formats,
		})
	}
	return map[string]any{
		"schema_draft": output.SchemaDraft202012,
		"count":        len(items),
		"schemas":      items,
	}
}

func writeSchemaCatalog(w io.Writer) error {
	return writeSchemaValue(w, buildSchemaCatalog())
}

func buildAllSchemasBundle() map[string]any {
	entries := registeredSchemas()
	schemas := make(map[string]*output.JSONSchema, len(entries))
	for _, e := range entries {
		schemas[e.Key] = buildEntrySchema(e)
	}
	return map[string]any{
		"schema_draft": output.SchemaDraft202012,
		"count":        len(schemas),
		"schemas":      schemas,
	}
}

func buildEntrySchema(e schemaEntry) *output.JSONSchema {
	s := e.Build()
	return output.FinalizeRootSchema(e.Slug, e.Title, e.Command, e.Description, e.Streaming, e.Formats, s)
}

func normalizeSchemaKey(raw string) string {
	s := strings.TrimSpace(strings.ToLower(raw))
	s = strings.TrimPrefix(s, "slb ")
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// LookupCommandSchema resolves a command path or alias (e.g. "pending",
// "daemon status", "daemon-status", "config show") to its JSON Schema.
func LookupCommandSchema(name string) (*output.JSONSchema, error) {
	norm := normalizeSchemaKey(name)
	if norm == "" {
		return nil, fmt.Errorf("command name is required (run 'slb schema' to list supported commands)")
	}
	entries := registeredSchemas()
	for _, e := range entries {
		if e.Key == norm || e.Slug == norm {
			return buildEntrySchema(e), nil
		}
		for _, alias := range e.Aliases {
			if normalizeSchemaKey(alias) == norm {
				return buildEntrySchema(e), nil
			}
		}
	}
	// Try matching with hyphens replaced by spaces (e.g. "daemon-status" -> "daemon status").
	spaced := strings.ReplaceAll(norm, "-", " ")
	if spaced != norm {
		for _, e := range entries {
			if e.Key == spaced {
				return buildEntrySchema(e), nil
			}
			for _, alias := range e.Aliases {
				if normalizeSchemaKey(alias) == spaced {
					return buildEntrySchema(e), nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no JSON schema defined for command %q (run 'slb schema' to list supported commands)", name)
}

// HasCommandSchema reports whether a canonical Cobra command path has a registered schema.
func HasCommandSchema(cmdPath string) bool {
	_, err := LookupCommandSchema(cmdPath)
	return err == nil
}

// RegisteredSchemaKeys returns all canonical schema keys in sorted order.
func RegisteredSchemaKeys() []string {
	entries := registeredSchemas()
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	return keys
}

func policyProps() map[string]*output.JSONSchema {
	return map[string]*output.JSONSchema{
		"approval_policy":       output.EnumSchema("Configured or recorded approval policy mode.", approvalPolicyValues...),
		"approval_policy_label": output.StringSchema("Human-readable description of the approval policy assurance boundary."),
	}
}

func withPolicy(props map[string]*output.JSONSchema) map[string]*output.JSONSchema {
	for k, v := range policyProps() {
		if _, exists := props[k]; !exists {
			props[k] = v
		}
	}
	return props
}

func reviewSummarySchema() *output.JSONSchema {
	return output.ObjectSchema(
		"Review decision recorded against a request.",
		withPolicy(map[string]*output.JSONSchema{
			"assurance":  output.EnumSchema("Evidence assurance level for this review.", reviewAssuranceValues...),
			"review_id":  output.StringSchema("Unique identifier of the review record."),
			"reviewer":   output.StringSchema("Agent name of the reviewer."),
			"model":      output.StringSchema("Model identifier reported by the reviewer session."),
			"decision":   output.EnumSchema("Review decision recorded.", "approve", "reject"),
			"comments":   output.StringSchema("Optional review comments."),
			"created_at": output.DateTimeSchema("RFC3339 timestamp when the review was recorded."),
		}),
		"approval_policy", "approval_policy_label", "assurance", "review_id", "reviewer", "model", "decision", "created_at",
	)
}

func reviewDetailItemSchema() *output.JSONSchema {
	return output.ObjectSchema(
		"Review decision summary returned by review show.",
		withPolicy(map[string]*output.JSONSchema{
			"assurance":      output.EnumSchema("Evidence assurance level for this review.", reviewAssuranceValues...),
			"id":             output.StringSchema("Unique identifier of the review record."),
			"reviewer_agent": output.StringSchema("Agent name of the reviewer."),
			"reviewer_model": output.StringSchema("Model identifier reported by the reviewer session."),
			"decision":       output.EnumSchema("Review decision recorded.", "approve", "reject"),
			"comments":       output.StringSchema("Optional review comments."),
			"created_at":     output.DateTimeSchema("RFC3339 timestamp when the review was recorded."),
		}),
		"approval_policy", "approval_policy_label", "assurance", "id", "reviewer_agent", "reviewer_model", "decision", "created_at",
	)
}

func patternProposalSchema() *output.JSONSchema {
	return output.ObjectSchema(
		"Pending pattern change proposal awaiting human review in TUI.",
		map[string]*output.JSONSchema{
			"status":        output.EnumSchema("Submission status of the proposal.", "pending", "suggested"),
			"review_status": output.StringSchema("Persisted review status of the pattern proposal."),
			"id":            output.IntSchema("Unique numeric identifier of the pattern change proposal."),
			"request_id":    output.IntSchema("Numeric alias of the pattern change proposal identifier."),
			"pattern":       output.StringSchema("Regular expression pattern proposed."),
			"tier":          output.StringSchema("Target risk tier for the pattern proposal (may be empty if unmatched)."),
			"reason":        output.StringSchema("Justification supplied for the proposal."),
			"message":       output.StringSchema("Human-readable status message describing next steps."),
		},
		"status", "review_status", "id", "request_id", "pattern", "tier", "reason", "message",
	)
}

func sessionViewSchema() *output.JSONSchema {
	return output.ObjectSchema(
		"Active agent session record.",
		map[string]*output.JSONSchema{
			"session_id":     output.StringSchema("Unique identifier of the session."),
			"agent_name":     output.StringSchema("Name of the agent associated with this session."),
			"program":        output.StringSchema("Client program name (for example claude-code or codex-cli)."),
			"model":          output.StringSchema("Model identifier reported by the session."),
			"project_path":   output.StringSchema("Project root path bound to the session."),
			"started_at":     output.DateTimeSchema("RFC3339 timestamp when the session started."),
			"last_active_at": output.DateTimeSchema("RFC3339 timestamp of the last recorded heartbeat or activity."),
		},
		"session_id", "agent_name", "program", "model", "project_path", "started_at", "last_active_at",
	)
}

func executionOutcomeSchema() *output.JSONSchema {
	return output.SchemaFromType(reflect.TypeOf(executionOutcome{}), map[string]string{
		"approval_policy":       "Recorded approval policy mode governing the request.",
		"approval_policy_label": "Human-readable description of the approval policy assurance boundary.",
		"assurance":             "Recorded execution evidence assurance level.",
		"request_id":            "Identifier of the executed request.",
		"status":                "Outcome status reported for the request execution.",
		"executed":              "True when the command process actually started.",
		"exit_code":             "Process exit code, or -1 if the command did not execute.",
		"duration_ms":           "Elapsed execution duration in milliseconds.",
		"log_path":              "Filesystem path to the captured execution log.",
		"output":                "Captured combined command output when available.",
		"timed_out":             "True when command execution exceeded its timeout.",
		"error":                 "Error message when execution or persistence failed.",
	})
}

func classificationSchema() *output.JSONSchema {
	segSchema := output.ObjectSchema(
		"Individual shell command segment that triggered classification.",
		map[string]*output.JSONSchema{
			"segment":         output.StringSchema("Extracted shell command segment."),
			"tier":            output.EnumSchema("Risk tier assigned to this segment.", riskTierValues...),
			"matched_pattern": output.StringSchema("Pattern or rule that matched this segment."),
		},
		"segment", "tier", "matched_pattern",
	)
	return output.ObjectSchema(
		"Command risk classification result.",
		map[string]*output.JSONSchema{
			"command":          output.StringSchema("Original command string evaluated."),
			"needs_approval":   output.BoolSchema("True when the command requires review before execution."),
			"is_safe":          output.BoolSchema("True when the command is classified as safe to skip review."),
			"min_approvals":    output.IntSchema("Minimum number of approvals required for the matched tier."),
			"tier":             output.NullableEnumSchema("Matched risk tier, or null when no rule matched.", riskTierValues...),
			"matched_pattern":  output.StringSchema("Regex pattern or semantic rule that determined the tier."),
			"parse_error":      output.BoolSchema("True when shell parsing failed and classification fell back conservatively."),
			"matched_segments": output.ArraySchema("Per-segment matches for compound shell commands.", segSchema),
		},
		"command", "needs_approval", "is_safe", "min_approvals", "tier",
	)
}

func configFieldDescriptions() map[string]string {
	return map[string]string{
		"General":                     "Core approval policy, quorum, timeout, dry-run, and rollback settings.",
		"Daemon":                      "Background approval daemon IPC, TCP, PID, and logging settings.",
		"RateLimits":                  "Per-session pending and per-minute request rate-limiting controls.",
		"Notifications":               "Blocked-command and lifecycle notification delivery settings.",
		"History":                     "SQLite history, Git audit export, and retention configuration.",
		"Patterns":                    "Per-tier risk classification rules and quorum overrides.",
		"Integrations":                "External tool integration toggles (Agent Mail and Claude Code hooks).",
		"Agents":                      "Agent allowlist and blocklist controls.",
		"ApprovalPolicy":              "Approval assurance policy: independent (default) or same_host.",
		"MinApprovals":                "Default minimum number of approvals required.",
		"RequireDifferentModel":       "Require reviewer sessions to report a different model than the requester.",
		"DifferentModelTimeoutSecs":   "Seconds to wait before relaxing different-model requirement when configured.",
		"ConflictResolution":          "Conflict policy when rejections occur: any_rejection_blocks, first_wins, or human_breaks_tie.",
		"RequestTimeoutSecs":          "Seconds before a pending request times out.",
		"ApprovalTTLMins":             "Minutes an approval remains valid before expiring.",
		"ApprovalTTLCriticalMins":     "Minutes an approval remains valid for CRITICAL requests.",
		"TimeoutAction":               "Action taken on request timeout: escalate, auto_reject, or auto_approve_warn.",
		"EnableDryRun":                "Enable bounded preflight dry-run preview capture before request admission.",
		"EnableRollbackCapture":       "Capture rollback snapshots before executing approved commands.",
		"MaxRollbackSizeMB":           "Maximum rollback capture size in megabytes.",
		"CrossProjectReviews":         "Allow review_pool allowlisted reviewers to review requests across projects.",
		"ReviewPool":                  "Allowlist of reviewer agent names eligible for cross-project reviews.",
		"UseFileWatcher":              "Use filesystem watchers in the daemon for pending request updates.",
		"IPCSocket":                   "Override path for the Unix domain IPC socket.",
		"TCPAddr":                     "Optional TCP bind address for remote daemon access.",
		"TCPRequireAuth":              "Require authentication for TCP daemon requests.",
		"TCPAllowedIPs":               "Allowlisted client IP addresses or CIDRs for TCP access.",
		"LogLevel":                    "Daemon log verbosity level.",
		"PIDFile":                     "Override path for the daemon PID file.",
		"MaxPendingPerSession":        "Maximum concurrent pending requests allowed per session.",
		"MaxRequestsPerMinute":        "Maximum new requests per minute allowed per session.",
		"RateLimitAction":             "Behavior when rate limit is reached: reject, queue, or warn.",
		"Blocked":                     "Notification settings for blocked-command hook alerts.",
		"Requests":                    "Notification settings for request lifecycle observations.",
		"DesktopEnabled":              "Enable local desktop notifications.",
		"DesktopDelaySecs":            "Delay in seconds before triggering desktop notifications.",
		"WebhookURL":                  "Optional webhook URL for notification delivery.",
		"EmailEnabled":                "Enable email notification delivery.",
		"DatabasePath":                "Path to the SQLite state database.",
		"GitRepoPath":                 "Path to local Git repository for immutable journal history export.",
		"RetentionDays":               "Days to retain historical records before pruning.",
		"AutoGitCommit":               "Automatically export committed request events to Git history.",
		"Critical":                    "CRITICAL tier pattern and quorum settings.",
		"Dangerous":                   "DANGEROUS tier pattern and quorum settings.",
		"Caution":                     "CAUTION tier pattern and auto-approval delay settings.",
		"Safe":                        "SAFE tier allowlist patterns.",
		"DynamicQuorum":               "Adjust required approvals based on active reviewers.",
		"DynamicQuorumFloor":          "Minimum quorum floor when dynamic quorum is enabled.",
		"AutoApproveDelaySeconds":     "Seconds before zero-review CAUTION requests auto-approve.",
		"AgentMailEnabled":            "Enable MCP Agent Mail notifications.",
		"AgentMailThread":             "Agent Mail thread identifier for SLB notifications.",
		"ClaudeHooksEnabled":          "Enable Claude Code PreToolUse hook integration.",
		"HookCautionAction":           "Hook action for CAUTION commands: block or ask.",
		"HookQueryTimeoutMS":          "Timeout in milliseconds for native hook daemon queries.",
		"TrustedSelfApprove":          "Deprecated legacy list; self-approval remains forbidden.",
		"TrustedSelfApproveDelaySecs": "Legacy delay setting retained for config compatibility.",
	}
}

func preflightReportSchema() *output.JSONSchema {
	return output.SchemaFromType(reflect.TypeOf(core.PreflightReport{}), map[string]string{
		"status":       "Preflight dry-run status (disabled, completed, failed, skipped, etc.).",
		"command_hash": "SHA-256 hash of the command evaluated by preflight.",
		"command":      "Preview command executed during preflight, if any.",
		"started_at":   "RFC3339 timestamp when preflight started.",
		"completed_at": "RFC3339 timestamp when preflight completed.",
		"duration_ms":  "Preflight execution duration in milliseconds.",
		"error":        "Error reported when preflight failed.",
	})
}

func rateLimitResultSchema() *output.JSONSchema {
	return output.SchemaFromType(reflect.TypeOf(core.RateLimitResult{}), map[string]string{
		"allowed":              "True when the session had sufficient rate-limit capacity.",
		"action":               "Configured rate-limit action taken (allow, reject, queue, warn).",
		"remaining_pending":    "Remaining concurrent pending request capacity for the session.",
		"remaining_per_minute": "Remaining per-minute request capacity for the session.",
		"reset_at":             "RFC3339 timestamp when the current rate-limit window resets.",
		"message":              "Advisory rate-limit status message, if any.",
	})
}

func requestAdmissionFailureSchema() *output.JSONSchema {
	return output.ObjectSchema(
		"Structured refusal when request admission is denied before creation.",
		map[string]*output.JSONSchema{
			"status":   {Type: "string", Const: "request_failed", Description: "Indicates admission failed before creating a request."},
			"code":     output.StringSchema("Machine-readable admission failure code (admission_failed, rate_limited, preflight_required, etc.)."),
			"created":  {Type: "boolean", Const: false, Description: "Always false when admission fails."},
			"executed": {Type: "boolean", Const: false, Description: "Always false when admission fails."},
			"error":    output.StringSchema("Human-readable explanation of why admission failed."),
			"rate_limit": output.ObjectSchema(
				"Rate-limit counters when admission failed due to rate limits.",
				map[string]*output.JSONSchema{
					"pending":        output.IntSchema("Current pending request count for the session."),
					"max_pending":    output.IntSchema("Configured maximum pending requests per session."),
					"recent":         output.IntSchema("Requests submitted in the last minute."),
					"max_per_minute": output.IntSchema("Configured maximum requests per minute."),
					"reset_at":       output.DateTimeSchema("RFC3339 timestamp when rate limit resets."),
				},
				"pending", "max_pending", "recent", "max_per_minute",
			),
		},
		"status", "code", "created", "executed", "error",
	)
}

func commandErrorSchema(desc string) *output.JSONSchema {
	return output.ObjectSchema(
		desc,
		map[string]*output.JSONSchema{
			"status":  output.StringSchema("Failure classification status."),
			"command": output.StringSchema("Command attempted."),
			"error":   output.StringSchema("Human-readable error explanation."),
		},
		"status", "command", "error",
	)
}

func gitAssessmentFieldDescriptions() map[string]string {
	return map[string]string{
		"operation":            "Git hook operation evaluated (commit, push, rebase).",
		"repository":           "Git repository root path.",
		"snapshot":             "Deterministic snapshot hash of the Git operation.",
		"tree":                 "Git tree OID bound to a commit snapshot.",
		"head":                 "Git HEAD OID observed during assessment.",
		"target_hash":          "SHA-256 digest of the push remote location.",
		"updates":              "Ref updates parsed from Git pre-push stdin.",
		"local_ref":            "Local Git reference name being pushed.",
		"local_oid":            "Local Git object ID being pushed.",
		"remote_ref":           "Remote Git reference name targeted by the push.",
		"remote_oid":           "Remote Git object ID observed before push.",
		"rebase":               "Hook-visible Git rebase snapshot details.",
		"scope":                "Rebase scope identifier.",
		"upstream":             "Upstream revision argument passed to rebase.",
		"upstream_oid":         "Resolved upstream object ID.",
		"root":                 "True when rebasing from repository root (--root).",
		"branch_argument":      "Explicit branch argument supplied to rebase, if any.",
		"branch_ref":           "Resolved Git reference name of the branch being rebased.",
		"branch_oid":           "Resolved object ID of the branch being rebased.",
		"head_ref":             "Current HEAD symbolic reference, if attached.",
		"ref_tips_hash":        "Hash of repository reference tips observed before rebase.",
		"reference_count":      "Number of repository references hashed into ref_tips_hash.",
		"candidate_commits":    "Commit OIDs visible as rebase candidates.",
		"candidates_truncated": "True when candidate_commits was truncated at the capture cap.",
		"unobserved":           "Rebase options or behaviors not exposed to Git's pre-rebase hook.",
		"reasons":              "Reasons why approval is or is not required.",
		"requires_approval":    "True when the Git operation requires SLB approval.",
	}
}

func gitAssessmentSchema() *output.JSONSchema {
	return output.SchemaFromType(reflect.TypeOf(slbgit.GitAssessment{}), gitAssessmentFieldDescriptions())
}

func gitHookStatusListSchema() *output.JSONSchema {
	item := output.SchemaFromType(reflect.TypeOf(slbgit.HookStatus{}), map[string]string{
		"name":       "Git hook name (pre-commit, pre-push, pre-rebase).",
		"path":       "Filesystem path to the hook inside Git's effective hooks directory.",
		"installed":  "True when a hook file exists at path.",
		"managed":    "True when the installed hook is managed by SLB.",
		"executable": "True when the hook file has executable permission bits.",
		"backup":     "Filesystem path to any preserved backup file.",
	})
	return output.ArraySchema("List of native Git hook installation statuses.", item)
}

func registeredSchemas() []schemaEntry {
	entries := []schemaEntry{
		{
			Key:         "version",
			Slug:        "version",
			Title:       "SLB Version Output",
			Command:     "slb version",
			Description: "Build metadata, Go runtime version, and resolved configuration/database paths.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Version and environment diagnostic payload emitted by slb version.",
					map[string]*output.JSONSchema{
						"version":       output.StringSchema("SLB semantic version string."),
						"commit":        output.StringSchema("Git commit SHA embedded at build time."),
						"build_date":    output.StringSchema("UTC build timestamp embedded at build time."),
						"go_version":    output.StringSchema("Go toolchain version used to build this binary."),
						"config_path":   output.StringSchema("Effective configuration file path."),
						"db_path":       output.StringSchema("Resolved project database path, or empty if unavailable."),
						"project_path":  output.StringSchema("Resolved project root path, or empty if unavailable."),
						"project_error": output.StringSchema("Diagnostic error if project discovery failed."),
					},
					"version", "commit", "build_date", "go_version", "config_path", "db_path", "project_path",
				)
			},
		},
		{
			Key:         "init",
			Slug:        "init",
			Title:       "SLB Init Output",
			Command:     "slb init",
			Description: "Paths created when initializing SLB in a project directory.",
			Formats:     []string{"json", "yaml"},
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Project initialization summary emitted by slb init.",
					map[string]*output.JSONSchema{
						"initialized": output.BoolSchema("Always true when initialization completes."),
						"path":        output.StringSchema("Absolute path to the created .slb directory."),
						"database":    output.StringSchema("Path to the initialized SQLite state database."),
						"config":      output.StringSchema("Path to the project config.toml file."),
						"directories": output.ArraySchema("Subdirectories created under .slb.", output.StringSchema("Subdirectory name.")),
					},
					"initialized", "path", "database", "config", "directories",
				)
			},
		},
		{
			Key:         "pending",
			Slug:        "pending",
			Title:       "SLB Pending Requests Output",
			Command:     "slb pending",
			Description: "Array of pending and escalated command requests awaiting review.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				item := output.ObjectSchema(
					"Pending or escalated request awaiting review.",
					withPolicy(map[string]*output.JSONSchema{
						"request_id":       output.StringSchema("Unique identifier of the request."),
						"status":           output.EnumSchema("Current lifecycle status of the request.", requestStatusValues...),
						"command":          output.StringSchema("Raw command text submitted for approval."),
						"command_redacted": output.StringSchema("Redacted command display text when sensitive values were detected."),
						"risk_tier":        output.EnumSchema("Assigned risk tier for the command.", riskTierValues...),
						"min_approvals":    output.IntSchema("Minimum number of approvals required."),
						"requestor_agent":  output.StringSchema("Agent name that submitted the request."),
						"requestor_model":  output.StringSchema("Model identifier reported by the requester session."),
						"project_path":     output.StringSchema("Project root path owning the request."),
						"reason":           output.StringSchema("Requester justification for running the command."),
						"created_at":       output.DateTimeSchema("RFC3339 timestamp when the request was created."),
						"expires_at":       output.DateTimeSchema("RFC3339 timestamp when the request expires."),
					}),
					"approval_policy", "approval_policy_label", "request_id", "status", "command",
					"risk_tier", "min_approvals", "requestor_agent", "requestor_model", "project_path", "created_at",
				)
				return output.ArraySchema("List of pending and escalated requests.", item)
			},
		},
		{
			Key:         "config",
			Slug:        "config",
			Title:       "SLB Effective Configuration Output",
			Command:     "slb config",
			Aliases:     []string{"config show"},
			Description: "Merged effective SLB configuration across defaults, user config, project config, and environment.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.SchemaFromType(reflect.TypeOf(config.Config{}), configFieldDescriptions())
			},
		},
		{
			Key:         "config get",
			Slug:        "config-get",
			Title:       "SLB Config Get Output",
			Command:     "slb config get",
			Description: "Single configuration key and its resolved effective value.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Configuration key/value lookup response.",
					map[string]*output.JSONSchema{
						"key":   output.StringSchema("Dotted configuration key requested."),
						"value": output.AnySchema("Resolved value for the configuration key."),
					},
					"key", "value",
				)
			},
		},
		{
			Key:         "config set",
			Slug:        "config-set",
			Title:       "SLB Config Set Output",
			Command:     "slb config set",
			Description: "Result of updating a configuration value in project or global config.toml.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Configuration update response.",
					withPolicy(map[string]*output.JSONSchema{
						"path":  output.StringSchema("Path to the config.toml file that was updated."),
						"key":   output.StringSchema("Dotted configuration key updated."),
						"value": output.AnySchema("Parsed value written to configuration."),
					}),
					"path", "key", "value",
				)
			},
		},
		{
			Key:         "request",
			Slug:        "request",
			Title:       "SLB Request Output",
			Command:     "slb request",
			Description: "Result of submitting a command approval request (either skipped as safe or created/waited/executed).",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				skipped := output.ObjectSchema(
					"Response when a command is classified SAFE and skips request creation.",
					map[string]*output.JSONSchema{
						"status":  {Type: "string", Const: "skipped", Description: "Indicates request creation was skipped."},
						"reason":  output.StringSchema("Explanation of why approval was skipped."),
						"command": output.StringSchema("Command string that was evaluated."),
						"tier":    output.EnumSchema("Matched risk tier when classification was performed.", riskTierValues...),
					},
					"status", "reason", "command",
				)
				created := output.ObjectSchema(
					"Response when an approval request is created (and optionally waited on or executed).",
					withPolicy(map[string]*output.JSONSchema{
						"request_id":       output.StringSchema("Unique identifier of the created request."),
						"status":           output.StringSchema("Current or final request lifecycle status."),
						"tier":             output.EnumSchema("Assigned risk tier for the command.", riskTierValues...),
						"command":          output.StringSchema("Raw command string submitted."),
						"command_redacted": output.StringSchema("Redacted display command when secrets were masked."),
						"command_hash":     output.StringSchema("SHA-256 hash binding approvals to the exact command."),
						"min_approvals":    output.IntSchema("Minimum number of approvals required."),
						"created_at":       output.DateTimeSchema("RFC3339 timestamp when the request was created."),
						"expires_at":       output.DateTimeSchema("RFC3339 timestamp when the request expires."),
						"resolved_at":      output.DateTimeSchema("RFC3339 timestamp when a decision was reached (with --wait)."),
						"queued":           output.BoolSchema("True if admission waited for rate-limit queue capacity."),
						"queue_wait_ms":    output.IntSchema("Milliseconds spent waiting in the admission queue."),
						"preflight":        preflightReportSchema(),
						"rate_limit":       rateLimitResultSchema(),
						"wait_timed_out":   output.BoolSchema("True when --wait exceeded the client wait timeout."),
						"error":            output.StringSchema("Error message when waiting failed."),
						"executed":         output.BoolSchema("True when --execute ran the approved command."),
						"exit_code":        output.IntSchema("Executed child process exit code."),
						"duration_ms":      output.IntSchema("Execution duration in milliseconds."),
						"log_path":         output.StringSchema("Path to the execution log file."),
						"output":           output.StringSchema("Captured command output."),
						"timed_out":        output.BoolSchema("True when command execution timed out."),
						"assurance":        output.EnumSchema("Recorded execution evidence assurance (empty if execution failed before start).", append(append([]string(nil), reviewAssuranceValues...), "")...),
						"execution_error":  output.StringSchema("Error reported when execution failed or was denied."),
					}),
					"approval_policy", "approval_policy_label", "request_id", "status", "tier",
					"command", "command_hash", "min_approvals", "created_at", "queued",
				)
				cmdErr := commandErrorSchema("Structured error emitted when slb request fails before creating or executing a request.")
				return &output.JSONSchema{
					Description: "Either a safe-skipped response, a created approval request payload, an admission refusal, or a structured command error.",
					AnyOf:       []*output.JSONSchema{skipped, created, requestAdmissionFailureSchema(), cmdErr},
				}
			},
		},
		{
			Key:         "status",
			Slug:        "status",
			Title:       "SLB Request Status Output",
			Command:     "slb status",
			Description: "Current status, review counts, and review records for a specific request.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Detailed status payload for a single approval request.",
					withPolicy(map[string]*output.JSONSchema{
						"request_id":              output.StringSchema("Unique identifier of the request."),
						"command":                 output.StringSchema("Raw command text."),
						"command_redacted":        output.StringSchema("Redacted command display text when sensitive values were masked."),
						"command_hash":            output.StringSchema("SHA-256 command binding hash."),
						"cwd":                     output.StringSchema("Working directory bound to the command."),
						"risk_tier":               output.EnumSchema("Assigned risk tier.", riskTierValues...),
						"status":                  output.EnumSchema("Current lifecycle status.", requestStatusValues...),
						"min_approvals":           output.IntSchema("Minimum number of approvals required."),
						"require_different_model": output.BoolSchema("Whether reviewers must report a different model than the requester."),
						"requestor_agent":         output.StringSchema("Agent name that submitted the request."),
						"requestor_model":         output.StringSchema("Model reported by the requester."),
						"project_path":            output.StringSchema("Project root path owning the request."),
						"reason":                  output.StringSchema("Requester justification."),
						"expected_effect":         output.StringSchema("Stated expected effect of running the command."),
						"goal":                    output.StringSchema("Stated goal the command supports."),
						"safety_argument":         output.StringSchema("Stated safety argument for the command."),
						"created_at":              output.DateTimeSchema("RFC3339 creation timestamp."),
						"resolved_at":             output.DateTimeSchema("RFC3339 resolution timestamp."),
						"expires_at":              output.DateTimeSchema("RFC3339 request expiration timestamp."),
						"approval_expires_at":     output.DateTimeSchema("RFC3339 approval validity expiration timestamp."),
						"approval_count":          output.IntSchema("Number of approval reviews recorded."),
						"rejection_count":         output.IntSchema("Number of rejection reviews recorded."),
						"reviews":                 output.ArraySchema("List of recorded reviews.", reviewSummarySchema()),
					}),
					"approval_policy", "approval_policy_label", "request_id", "command", "command_hash",
					"risk_tier", "status", "min_approvals", "require_different_model", "requestor_agent",
					"requestor_model", "project_path", "created_at", "approval_count", "rejection_count", "reviews",
				)
			},
		},
		{
			Key:         "show",
			Slug:        "show",
			Title:       "SLB Show Request Output",
			Command:     "slb show",
			Description: "Complete inspection view of a request including command spec, justification, dry run, reviews, execution, rollback, and attachments.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				cmdObj := output.ObjectSchema(
					"Bound command specification.",
					map[string]*output.JSONSchema{
						"raw":                output.StringSchema("Raw command string."),
						"display_redacted":   output.StringSchema("Redacted command string for safe display."),
						"argv":               output.NullableArraySchema("Parsed argument vector when available.", output.StringSchema("Argument token.")),
						"cwd":                output.StringSchema("Bound working directory."),
						"shell":              output.BoolSchema("True when command runs via shell."),
						"hash":               output.StringSchema("SHA-256 hash of the bound command."),
						"contains_sensitive": output.BoolSchema("True when sensitive content was detected."),
					},
					"raw", "shell", "hash", "contains_sensitive",
				)
				justObj := output.ObjectSchema(
					"Structured justification provided by the requester.",
					map[string]*output.JSONSchema{
						"reason":          output.StringSchema("Primary justification for running the command."),
						"expected_effect": output.StringSchema("Expected effect on the system or repo."),
						"goal":            output.StringSchema("High-level goal this command serves."),
						"safety_argument": output.StringSchema("Argument explaining why execution is safe."),
					},
				)
				dryRunObj := output.ObjectSchema(
					"Advisory dry-run preview captured before admission.",
					map[string]*output.JSONSchema{
						"command": output.StringSchema("Preview command executed."),
						"output":  output.StringSchema("Captured preview output."),
					},
				)
				responsesObj := output.ObjectSchema(
					"Structured reviewer responses to justification prompts.",
					map[string]*output.JSONSchema{
						"reason_response": output.StringSchema("Reviewer comment on the stated reason."),
						"effect_response": output.StringSchema("Reviewer comment on expected effect."),
						"goal_response":   output.StringSchema("Reviewer comment on goal."),
						"safety_response": output.StringSchema("Reviewer comment on safety argument."),
					},
				)
				showReview := output.ObjectSchema(
					"Full signed review record.",
					withPolicy(map[string]*output.JSONSchema{
						"assurance":           output.EnumSchema("Evidence assurance level for this review.", reviewAssuranceValues...),
						"command_hash":        output.StringSchema("Command hash covered by the review signature."),
						"signature_version":   output.IntSchema("Review signature format version."),
						"review_id":           output.StringSchema("Unique review identifier."),
						"reviewer_session_id": output.StringSchema("Session ID of the reviewer."),
						"reviewer_agent":      output.StringSchema("Agent name of the reviewer."),
						"reviewer_model":      output.StringSchema("Model reported by the reviewer."),
						"decision":            output.EnumSchema("Recorded decision.", "approve", "reject"),
						"signature":           output.StringSchema("HMAC signature authenticating the review record."),
						"signature_timestamp": output.DateTimeSchema("RFC3339 timestamp signed into the review."),
						"responses":           responsesObj,
						"comments":            output.StringSchema("Free-form reviewer comments."),
						"created_at":          output.DateTimeSchema("RFC3339 timestamp when review was recorded."),
					}),
					"approval_policy", "approval_policy_label", "assurance", "review_id",
					"reviewer_session_id", "reviewer_agent", "reviewer_model", "decision", "signature_version", "created_at",
				)
				execObj := output.ObjectSchema(
					"Recorded execution result for the request.",
					withPolicy(map[string]*output.JSONSchema{
						"assurance":              output.EnumSchema("Execution evidence assurance.", reviewAssuranceValues...),
						"executed_at":            output.DateTimeSchema("RFC3339 timestamp when execution occurred."),
						"executed_by_session_id": output.StringSchema("Session ID that executed the request."),
						"executed_by_agent":      output.StringSchema("Agent name that executed the request."),
						"executed_by_model":      output.StringSchema("Model reported by the executing session."),
						"exit_code":              output.NullableIntSchema("Exit code returned by the command."),
						"duration_ms":            output.NullableIntSchema("Execution duration in milliseconds."),
						"log_path":               output.StringSchema("Path to the execution log file."),
					}),
					"approval_policy", "approval_policy_label", "assurance",
				)
				rollbackObj := output.ObjectSchema(
					"Captured rollback state metadata.",
					map[string]*output.JSONSchema{
						"path":           output.StringSchema("Filesystem path to rollback snapshot data."),
						"rolled_back_at": output.DateTimeSchema("RFC3339 timestamp when rollback was applied."),
					},
				)
				attachObj := output.ObjectSchema(
					"Context attachment stored with the request.",
					map[string]*output.JSONSchema{
						"type":     output.StringSchema("Attachment kind (file, context, diff, screenshot, etc.)."),
						"content":  output.StringSchema("Attachment content when --with-attachments is requested."),
						"metadata": output.MapSchema("Attachment metadata key/value pairs.", output.AnySchema("Metadata value.")),
					},
					"type",
				)
				return output.ObjectSchema(
					"Full request inspection document emitted by slb show.",
					withPolicy(map[string]*output.JSONSchema{
						"request_id":              output.StringSchema("Unique identifier of the request."),
						"project_path":            output.StringSchema("Project root path owning the request."),
						"command":                 cmdObj,
						"risk_tier":               output.EnumSchema("Assigned risk tier.", riskTierValues...),
						"status":                  output.EnumSchema("Current lifecycle status.", requestStatusValues...),
						"min_approvals":           output.IntSchema("Minimum required approvals."),
						"require_different_model": output.BoolSchema("Whether a different model is required for review."),
						"requestor_session_id":    output.StringSchema("Session ID of the requester."),
						"requestor_agent":         output.StringSchema("Agent name of the requester."),
						"requestor_model":         output.StringSchema("Model reported by the requester."),
						"justification":           justObj,
						"dry_run":                 dryRunObj,
						"reviews":                 output.ArraySchema("Recorded reviews when --with-reviews is enabled.", showReview),
						"execution":               execObj,
						"rollback":                rollbackObj,
						"attachments":             output.ArraySchema("Context attachments attached to the request.", attachObj),
						"created_at":              output.DateTimeSchema("RFC3339 creation timestamp."),
						"resolved_at":             output.DateTimeSchema("RFC3339 resolution timestamp."),
						"expires_at":              output.DateTimeSchema("RFC3339 request expiration timestamp."),
						"approval_expires_at":     output.DateTimeSchema("RFC3339 approval validity expiration timestamp."),
					}),
					"approval_policy", "approval_policy_label", "request_id", "project_path", "command",
					"risk_tier", "status", "min_approvals", "require_different_model",
					"requestor_session_id", "requestor_agent", "requestor_model", "justification", "created_at",
				)
			},
		},
		{
			Key:         "history",
			Slug:        "history",
			Title:       "SLB History Output",
			Command:     "slb history",
			Description: "Array of historical command approval requests matching filter criteria.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				item := output.ObjectSchema(
					"Historical request summary entry.",
					withPolicy(map[string]*output.JSONSchema{
						"execution_assurance": output.EnumSchema("Recorded execution evidence assurance, when executed.", reviewAssuranceValues...),
						"request_id":          output.StringSchema("Unique identifier of the request."),
						"command":             output.StringSchema("Command string (redacted when sensitive)."),
						"risk_tier":           output.EnumSchema("Assigned risk tier.", riskTierValues...),
						"status":              output.EnumSchema("Recorded lifecycle status.", requestStatusValues...),
						"requestor_agent":     output.StringSchema("Agent name that submitted the request."),
						"project_path":        output.StringSchema("Project root path owning the request."),
						"created_at":          output.DateTimeSchema("RFC3339 creation timestamp."),
						"resolved_at":         output.DateTimeSchema("RFC3339 resolution timestamp."),
					}),
					"approval_policy", "approval_policy_label", "request_id", "command",
					"risk_tier", "status", "requestor_agent", "project_path", "created_at",
				)
				return output.ArraySchema("List of historical requests.", item)
			},
		},
		{
			Key:         "history sync",
			Slug:        "history-sync",
			Title:       "SLB History Sync Output",
			Command:     "slb history sync",
			Description: "Result of exporting durable request journal metadata to local Git history.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.SchemaFromType(reflect.TypeOf(slbgit.HistorySyncResult{}), map[string]string{
					"repository":    "Path to the local Git repository receiving exported history.",
					"ref":           "Project-specific Git ref updated (under refs/slb/history/).",
					"commit":        "Git commit SHA created or retained for this sync.",
					"events":        "Number of journal events exported in this batch.",
					"last_sequence": "Highest journal sequence number exported.",
					"cursor":        "Opaque resume cursor persisted with this export.",
					"has_more":      "True when additional journal events remain to be exported.",
				})
			},
		},
		{
			Key:         "approve",
			Slug:        "approve",
			Title:       "SLB Approve Output",
			Command:     "slb approve",
			Description: "Result of recording an approval review on a request.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Recorded approval review outcome.",
					withPolicy(map[string]*output.JSONSchema{
						"assurance":              output.EnumSchema("Evidence assurance level of the recorded review.", reviewAssuranceValues...),
						"review_id":              output.StringSchema("Unique identifier of the new review record."),
						"request_id":             output.StringSchema("Identifier of the reviewed request."),
						"request_status":         output.EnumSchema("Current lifecycle status of the request after review.", requestStatusValues...),
						"decision":               {Type: "string", Const: "approve", Description: "Recorded decision value."},
						"approvals":              output.IntSchema("Total approval count after this review."),
						"rejections":             output.IntSchema("Total rejection count after this review."),
						"request_status_changed": output.BoolSchema("True if this review transitioned the request's status."),
						"new_request_status":     output.EnumSchema("New status when request_status_changed is true.", requestStatusValues...),
						"created_at":             output.DateTimeSchema("RFC3339 timestamp when the review was recorded."),
					}),
					"approval_policy", "approval_policy_label", "assurance", "review_id", "request_id",
					"request_status", "decision", "approvals", "rejections", "request_status_changed", "created_at",
				)
			},
		},
		{
			Key:         "reject",
			Slug:        "reject",
			Title:       "SLB Reject Output",
			Command:     "slb reject",
			Description: "Result of recording a rejection review on a request.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Recorded rejection review outcome.",
					withPolicy(map[string]*output.JSONSchema{
						"assurance":              output.EnumSchema("Evidence assurance level of the recorded review.", reviewAssuranceValues...),
						"review_id":              output.StringSchema("Unique identifier of the new review record."),
						"request_id":             output.StringSchema("Identifier of the reviewed request."),
						"request_status":         output.EnumSchema("Current lifecycle status of the request after review.", requestStatusValues...),
						"decision":               {Type: "string", Const: "reject", Description: "Recorded decision value."},
						"reason":                 output.StringSchema("Rejection rationale recorded by the reviewer."),
						"approvals":              output.IntSchema("Total approval count after this review."),
						"rejections":             output.IntSchema("Total rejection count after this review."),
						"request_status_changed": output.BoolSchema("True if this review transitioned the request's status."),
						"new_request_status":     output.EnumSchema("New status when request_status_changed is true.", requestStatusValues...),
						"created_at":             output.DateTimeSchema("RFC3339 timestamp when the review was recorded."),
					}),
					"approval_policy", "approval_policy_label", "assurance", "review_id", "request_id",
					"request_status", "decision", "reason", "approvals", "rejections", "request_status_changed", "created_at",
				)
			},
		},
		{
			Key:         "cancel",
			Slug:        "cancel",
			Title:       "SLB Cancel Output",
			Command:     "slb cancel",
			Description: "Confirmation payload returned when cancelling an unexecuted request.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Request cancellation receipt.",
					withPolicy(map[string]*output.JSONSchema{
						"request_id":    output.StringSchema("Identifier of the cancelled request."),
						"status":        {Type: "string", Const: "cancelled", Description: "Terminal cancelled status."},
						"cancelled_at":  output.DateTimeSchema("RFC3339 timestamp when cancellation was committed."),
						"review_notice": output.StringSchema("Approval policy label recorded on the request."),
					}),
					"approval_policy", "approval_policy_label", "request_id", "status", "cancelled_at", "review_notice",
				)
			},
		},
		{
			Key:         "execute",
			Slug:        "execute",
			Title:       "SLB Execute Output",
			Command:     "slb execute",
			Description: "Result of executing an approved request in the foreground or launching it in the background.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				fg := output.ObjectSchema(
					"Foreground execution outcome for an approved request.",
					withPolicy(map[string]*output.JSONSchema{
						"assurance":   output.EnumSchema("Execution evidence assurance level.", reviewAssuranceValues...),
						"request_id":  output.StringSchema("Identifier of the executed request."),
						"status":      output.StringSchema("Final execution status (executed, execution_failed, timed_out, or not_executed)."),
						"exit_code":   output.IntSchema("Exit code returned by the command, or -1 if not executed."),
						"duration_ms": output.IntSchema("Elapsed execution duration in milliseconds."),
						"log_path":    output.StringSchema("Path to the execution log file."),
						"output":      output.StringSchema("Captured command output."),
						"timed_out":   output.BoolSchema("True when command execution timed out."),
						"error":       output.StringSchema("Error message if execution failed."),
					}),
					"approval_policy", "approval_policy_label", "request_id", "status", "exit_code", "duration_ms", "log_path",
				)
				bgReceipt := output.SchemaFromType(reflect.TypeOf(background.Receipt{}), map[string]string{
					"approval_policy":       "Recorded approval policy governing the request.",
					"approval_policy_label": "Human-readable approval policy label.",
					"assurance":             "Execution evidence assurance level.",
					"version":               "Background launcher protocol version.",
					"request_id":            "Identifier of the launched request.",
					"command_hash":          "SHA-256 hash of the bound command.",
					"status":                "Background execution status.",
					"supervisor_pid":        "PID of the background supervisor process.",
					"pid":                   "PID of the launched command process.",
					"log_path":              "Path to the command output log.",
					"supervisor_log":        "Path to the background supervisor diagnostic log.",
					"error":                 "Error message if launch reported a fault.",
				})
				bgErr := output.ObjectSchema(
					"Background launch failure or ambiguous startup receipt.",
					withPolicy(map[string]*output.JSONSchema{
						"request_id":     output.StringSchema("Identifier of the request."),
						"status":         output.EnumSchema("Launch failure status.", "not_launched", "startup_unconfirmed"),
						"error":          output.StringSchema("Error describing why launch failed or could not be confirmed."),
						"supervisor_pid": output.IntSchema("Supervisor PID when startup could not be confirmed."),
						"supervisor_log": output.StringSchema("Supervisor log path when startup could not be confirmed."),
					}),
					"approval_policy", "approval_policy_label", "request_id", "status", "error",
				)
				return &output.JSONSchema{
					Description: "Foreground execution result or background launch receipt.",
					AnyOf:       []*output.JSONSchema{fg, bgReceipt, bgErr},
				}
			},
		},
		{
			Key:         "run",
			Slug:        "run",
			Title:       "SLB Run Output",
			Command:     "slb run",
			Description: "Atomic check, request, wait, and execute result (safe bypass, approved execution, or structured rejection/error).",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				safeRun := output.ObjectSchema(
					"Execution result for a SAFE command that bypassed approval.",
					map[string]*output.JSONSchema{
						"status":           output.StringSchema("Execution status (executed, execution_failed, timed_out)."),
						"executed":         output.BoolSchema("True when the command process started."),
						"command":          output.StringSchema("Command string executed."),
						"exit_code":        output.IntSchema("Command exit code."),
						"duration_ms":      output.IntSchema("Execution duration in milliseconds."),
						"log_path":         output.StringSchema("Path to the execution log file."),
						"tier":             output.EnumSchema("Risk tier assigned to the command.", riskTierValues...),
						"skipped_approval": output.BoolSchema("True indicating approval was skipped for a SAFE command."),
						"output":           output.StringSchema("Captured command output."),
						"error":            output.StringSchema("Error message when execution failed."),
					},
					"status", "executed", "command", "exit_code", "duration_ms", "log_path", "tier", "skipped_approval", "output",
				)
				yieldPending := output.ObjectSchema(
					"Pending approval receipt returned when slb run --yield submits a non-SAFE command.",
					withPolicy(map[string]*output.JSONSchema{
						"status":        {Type: "string", Const: "pending", Description: "Indicates the request is awaiting approval."},
						"request_id":    output.StringSchema("Unique identifier of the created approval request."),
						"tier":          output.EnumSchema("Assigned risk tier for the command.", riskTierValues...),
						"min_approvals": output.IntSchema("Minimum number of approvals required."),
						"message":       output.StringSchema("Human-readable guidance on checking status."),
						"queued":        output.BoolSchema("True if admission waited for rate-limit queue capacity."),
						"queue_wait_ms": output.IntSchema("Milliseconds spent waiting in the admission queue."),
						"preflight":     preflightReportSchema(),
						"rate_limit":    rateLimitResultSchema(),
					}),
					"approval_policy", "approval_policy_label", "status", "request_id", "tier", "min_approvals", "message", "queued",
				)
				runErr := commandErrorSchema("Structured error emitted when slb run cannot complete approval or execution.")
				return &output.JSONSchema{
					Description: "Outcome of slb run: direct safe execution, --yield pending receipt, approved request execution, admission refusal, or structured failure.",
					AnyOf:       []*output.JSONSchema{safeRun, yieldPending, executionOutcomeSchema(), requestAdmissionFailureSchema(), runErr},
				}
			},
		},
		{
			Key:         "review",
			Slug:        "review",
			Title:       "SLB Review List Output",
			Command:     "slb review list",
			Aliases:     []string{"review list"},
			Description: "Array of pending and escalated request summaries for reviewers.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				item := output.ObjectSchema(
					"Compact request summary for reviewer queues.",
					withPolicy(map[string]*output.JSONSchema{
						"id":              output.StringSchema("Unique identifier of the request."),
						"status":          output.EnumSchema("Current lifecycle status.", requestStatusValues...),
						"command":         output.StringSchema("Display command text (redacted and truncated to 60 chars)."),
						"risk_tier":       output.EnumSchema("Assigned risk tier.", riskTierValues...),
						"requestor_agent": output.StringSchema("Agent name that submitted the request."),
						"min_approvals":   output.IntSchema("Minimum number of approvals required."),
						"created_at":      output.DateTimeSchema("RFC3339 creation timestamp."),
						"project_path":    output.StringSchema("Project path when --all-projects or --review-pool is set."),
					}),
					"approval_policy", "approval_policy_label", "id", "status", "command",
					"risk_tier", "requestor_agent", "min_approvals", "created_at",
				)
				return output.ArraySchema("List of reviewable request summaries.", item)
			},
		},
		{
			Key:         "review show",
			Slug:        "review-show",
			Title:       "SLB Review Show Output",
			Command:     "slb review show",
			Description: "Detailed reviewer view of a single request and its current votes.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Detailed request view emitted by slb review show.",
					withPolicy(map[string]*output.JSONSchema{
						"id":                            output.StringSchema("Unique identifier of the request."),
						"status":                        output.EnumSchema("Current lifecycle status.", requestStatusValues...),
						"risk_tier":                     output.EnumSchema("Assigned risk tier.", riskTierValues...),
						"command":                       output.StringSchema("Display command text (redacted when sensitive)."),
						"command_hash":                  output.StringSchema("SHA-256 command hash."),
						"cwd":                           output.StringSchema("Bound working directory."),
						"project_path":                  output.StringSchema("Project root path owning the request."),
						"requestor_agent":               output.StringSchema("Agent name that submitted the request."),
						"requestor_model":               output.StringSchema("Model reported by the requester."),
						"justification_reason":          output.StringSchema("Primary justification for the command."),
						"justification_expected_effect": output.StringSchema("Expected effect stated by the requester."),
						"justification_goal":            output.StringSchema("Goal stated by the requester."),
						"justification_safety_argument": output.StringSchema("Safety argument stated by the requester."),
						"min_approvals":                 output.IntSchema("Minimum number of approvals required."),
						"current_approvals":             output.IntSchema("Current count of recorded approvals."),
						"current_rejections":            output.IntSchema("Current count of recorded rejections."),
						"require_different_model":       output.BoolSchema("Whether reviewers must report a different model."),
						"reviews":                       output.ArraySchema("Reviews recorded so far.", reviewDetailItemSchema()),
						"dry_run_command":               output.StringSchema("Advisory dry-run command executed before admission."),
						"dry_run_output":                output.StringSchema("Advisory dry-run output captured before admission."),
						"created_at":                    output.DateTimeSchema("RFC3339 creation timestamp."),
						"expires_at":                    output.DateTimeSchema("RFC3339 request expiration timestamp."),
					}),
					"approval_policy", "approval_policy_label", "id", "status", "risk_tier", "command",
					"command_hash", "cwd", "project_path", "requestor_agent", "requestor_model",
					"justification_reason", "min_approvals", "current_approvals", "current_rejections",
					"require_different_model", "created_at",
				)
			},
		},
		{
			Key:         "watch",
			Slug:        "watch",
			Title:       "SLB Watch Stream Event",
			Command:     "slb watch",
			Description: "Single NDJSON event line emitted by slb watch (request state observation or watch_degraded fallback notice).",
			Streaming:   true,
			Formats:     ndjsonFormats,
			Build: func() *output.JSONSchema {
				reqEvent := output.SchemaFromType(reflect.TypeOf(daemon.RequestStreamEvent{}), map[string]string{
					"event":               "Event type (request_pending, request_approved, request_rejected, etc.).",
					"request_id":          "Identifier of the observed request.",
					"project_path":        "Project root path owning the request.",
					"status":              "Observed lifecycle status of the request.",
					"risk_tier":           "Assigned risk tier.",
					"command":             "Display command string.",
					"command_hash":        "SHA-256 command binding hash.",
					"requestor":           "Requester agent name.",
					"min_approvals":       "Required approval count.",
					"approvals":           "Submitted approval count.",
					"rejections":          "Submitted rejection count.",
					"approval_policy":     "Approval policy mode recorded on the request.",
					"review_notice":       "Human-readable approval policy notice.",
					"execution_assurance": "Execution evidence assurance level.",
					"approved_by":         "Most recent approving reviewer.",
					"rejected_by":         "Most recent rejecting reviewer.",
					"reason":              "Request or decision rationale.",
					"exit_code":           "Observed execution exit code.",
					"created_at":          "RFC3339 creation timestamp.",
					"resolved_at":         "RFC3339 resolution timestamp.",
					"executed_at":         "RFC3339 execution timestamp.",
					"observed_at":         "RFC3339 observation timestamp.",
					"active_sessions":     "Active session count when reported.",
				})
				degraded := output.ObjectSchema(
					"Transport fallback event emitted when daemon IPC subscription degrades to database polling.",
					map[string]*output.JSONSchema{
						"event": {Type: "string", Const: "watch_degraded", Description: "Indicates transition to polling mode."},
						"mode":  output.StringSchema("Active fallback transport mode (polling)."),
						"error": output.StringSchema("Diagnostic error explaining why daemon IPC ended."),
					},
					"event", "mode", "error",
				)
				return &output.JSONSchema{
					Description: "NDJSON line emitted by slb watch: either a request state event or a watch_degraded notice.",
					AnyOf:       []*output.JSONSchema{reqEvent, degraded},
				}
			},
		},
		{
			Key:         "events",
			Slug:        "events",
			Title:       "SLB Durable Request Event Stream Record",
			Command:     "slb events",
			Description: "Single NDJSON record emitted by slb events (durable journal event or page checkpoint).",
			Streaming:   true,
			Formats:     ndjsonFormats,
			Build: func() *output.JSONSchema {
				eventSchema := output.SchemaFromType(reflect.TypeOf(db.RequestEvent{}), map[string]string{
					"version":                   "Durable journal schema version.",
					"sequence":                  "Monotonic journal sequence number.",
					"event":                     "Lifecycle event kind (request_created, request_reviewed, request_baseline, etc.).",
					"project_path":              "Absolute project root path for the event.",
					"request_id":                "Identifier of the affected request.",
					"occurred_at":               "RFC3339 timestamp when the event was committed.",
					"state":                     "Snapshot of request metadata at this sequence.",
					"review":                    "Review metadata when the event records a review.",
					"previous_status":           "Prior status before this state transition.",
					"cursor":                    "Opaque resume cursor binding database, project, and sequence.",
					"approval_policy":           "Approval policy governing the request or review.",
					"execution_approval_policy": "Approval policy in force during execution.",
					"execution_assurance":       "Execution evidence assurance level.",
					"command_hash":              "SHA-256 hash of the bound command.",
					"status":                    "Request status at this event.",
					"risk_tier":                 "Assigned risk tier.",
					"requestor_session_id":      "Session ID of the requester.",
					"min_approvals":             "Minimum approvals required.",
					"require_different_model":   "Whether different-model review is required.",
					"approvals":                 "Recorded approval count.",
					"rejections":                "Recorded rejection count.",
					"created_at":                "RFC3339 request creation timestamp.",
					"expires_at":                "RFC3339 request expiration timestamp, or null.",
					"resolved_at":               "RFC3339 resolution timestamp, or null.",
					"approval_expires_at":       "RFC3339 approval expiration timestamp, or null.",
					"executor_session_id":       "Session ID that executed the request, or null.",
					"executed_at":               "RFC3339 execution timestamp, or null.",
					"exit_code":                 "Recorded command exit code, or null.",
					"duration_ms":               "Recorded execution duration in milliseconds, or null.",
					"rolled_back_at":            "RFC3339 rollback timestamp, or null.",
					"id":                        "Review record identifier.",
					"session_id":                "Reviewer session identifier.",
					"decision":                  "Recorded review decision.",
					"assurance":                 "Review evidence assurance level.",
					"signature_version":         "Review signature format version.",
				})
				checkpoint := output.ObjectSchema(
					"Page boundary checkpoint record carrying the resume cursor and backlog flag.",
					map[string]*output.JSONSchema{
						"version":      output.IntSchema("Durable journal schema version."),
						"event":        {Type: "string", Const: "checkpoint", Description: "Checkpoint marker event kind."},
						"project_path": output.StringSchema("Absolute project root path."),
						"cursor":       output.StringSchema("Opaque resume cursor for subsequent --after queries."),
						"has_more":     output.BoolSchema("True when additional journal events remain beyond this page."),
					},
					"version", "event", "project_path", "cursor", "has_more",
				)
				return &output.JSONSchema{
					Description: "NDJSON line emitted by slb events: either a durable RequestEvent or a checkpoint.",
					AnyOf:       []*output.JSONSchema{eventSchema, checkpoint},
				}
			},
		},
		{
			Key:         "check",
			Slug:        "check",
			Title:       "SLB Command Classification Output",
			Command:     "slb check",
			Aliases:     []string{"patterns test"},
			Description: "Risk tier classification result emitted by slb check and slb patterns test.",
			Formats:     standardFormats,
			Build:       classificationSchema,
		},
		{
			Key:         "patterns list",
			Slug:        "patterns-list",
			Title:       "SLB Patterns List Output",
			Command:     "slb patterns list",
			Aliases:     []string{"patterns"},
			Description: "Map of risk tier names to arrays of active classification patterns.",
			Formats:     []string{"json"},
			Build: func() *output.JSONSchema {
				item := output.ObjectSchema(
					"Single classification pattern entry.",
					map[string]*output.JSONSchema{
						"pattern":     output.StringSchema("Regular expression matched against normalized commands."),
						"description": output.StringSchema("Human-readable explanation of what the pattern matches."),
						"source":      output.StringSchema("Origin of the pattern (builtin, config, or custom database rule)."),
					},
					"pattern",
				)
				return output.MapSchema(
					"Active patterns grouped by tier name (safe, critical, dangerous, caution).",
					output.ArraySchema("Patterns configured for this tier.", item),
				)
			},
		},
		{
			Key:         "patterns add",
			Slug:        "patterns-add",
			Title:       "SLB Patterns Add Output",
			Command:     "slb patterns add",
			Description: "Result of adding a restrictive pattern immediately or queueing a SAFE pattern proposal for human review.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				added := output.ObjectSchema(
					"Immediate addition of a restrictive pattern (or idempotent confirmation when it already exists).",
					map[string]*output.JSONSchema{
						"id":       output.IntSchema("Database ID of the added or existing custom pattern."),
						"status":   output.EnumSchema("Indicates whether the pattern was newly added or already existed.", "added", "already_exists"),
						"pattern":  output.StringSchema("Regular expression pattern added."),
						"tier":     output.EnumSchema("Restrictive risk tier assigned.", "critical", "dangerous", "caution"),
						"reason":   output.StringSchema("Justification supplied for adding the pattern."),
						"added_by": output.StringSchema("Actor category that added the pattern."),
					},
					"id", "status", "pattern", "tier", "reason", "added_by",
				)
				return &output.JSONSchema{
					Description: "Either immediate restrictive pattern addition or queued SAFE pattern proposal.",
					AnyOf:       []*output.JSONSchema{added, patternProposalSchema()},
				}
			},
		},
		{
			Key:         "patterns remove",
			Slug:        "patterns-remove",
			Title:       "SLB Patterns Remove Blocked Output",
			Command:     "slb patterns remove",
			Description: "Structured refusal emitted when an agent attempts direct pattern removal.",
			Formats:     []string{"json"},
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Refusal payload indicating direct pattern removal is blocked for agents.",
					map[string]*output.JSONSchema{
						"error":   {Type: "string", Const: "pattern_removal_blocked", Description: "Machine-readable refusal code."},
						"message": output.StringSchema("Human-readable guidance directing to TUI or request-removal."),
					},
					"error", "message",
				)
			},
		},
		{
			Key:         "patterns request-removal",
			Slug:        "patterns-request-removal",
			Title:       "SLB Pattern Removal Proposal Output",
			Command:     "slb patterns request-removal",
			Description: "Confirmation of a pending custom-pattern removal request for human review.",
			Formats:     standardFormats,
			Build:       patternProposalSchema,
		},
		{
			Key:         "patterns suggest",
			Slug:        "patterns-suggest",
			Title:       "SLB Pattern Suggestion Output",
			Command:     "slb patterns suggest",
			Description: "Confirmation of a suggested pattern change recorded for human review.",
			Formats:     standardFormats,
			Build:       patternProposalSchema,
		},
		{
			Key:         "patterns export",
			Slug:        "patterns-export",
			Title:       "SLB Patterns Export Output",
			Command:     "slb patterns export",
			Description: "Export summary when writing to --output-file, or exported pattern catalog payload.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				fileSummary := output.ObjectSchema(
					"Summary emitted when exporting patterns to a file with --output-file.",
					map[string]*output.JSONSchema{
						"status": {Type: "string", Const: "exported", Description: "Export completion status."},
						"format": output.StringSchema("Requested export format (json, yaml, claude-hook)."),
						"file":   output.StringSchema("Destination file path written."),
						"hash":   output.StringSchema("SHA-256 hash of the exported pattern set."),
						"count":  output.IntSchema("Total number of patterns exported."),
					},
					"status", "format", "file", "hash", "count",
				)
				rawExport := output.SchemaFromType(reflect.TypeOf(core.PatternExport{}), map[string]string{
					"version":       "Pattern export format version.",
					"sha256":        "SHA-256 digest of the canonical pattern set.",
					"generated_at":  "RFC3339 timestamp when export was generated.",
					"metadata":      "Summary counts and metadata for the exported patterns.",
					"pattern_count": "Total count of exported patterns.",
					"tier_counts":   "Per-tier count of exported patterns.",
				})
				return &output.JSONSchema{
					Description: "File export confirmation or exported PatternExport document.",
					AnyOf:       []*output.JSONSchema{fileSummary, rawExport},
				}
			},
		},
		{
			Key:         "patterns version",
			Slug:        "patterns-version",
			Title:       "SLB Patterns Version Output",
			Command:     "slb patterns version",
			Description: "Active pattern engine version, SHA-256 fingerprint, and per-tier counts.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Pattern set version and digest summary.",
					map[string]*output.JSONSchema{
						"version":       output.StringSchema("Pattern engine schema version."),
						"sha256":        output.StringSchema("SHA-256 hash of the effective pattern set."),
						"pattern_count": output.IntSchema("Total number of active patterns."),
						"tier_counts":   output.MapSchema("Number of active patterns per tier.", output.IntSchema("Count of patterns in tier.")),
					},
					"version", "sha256", "pattern_count", "tier_counts",
				)
			},
		},
		{
			Key:         "session start",
			Slug:        "session-start",
			Title:       "SLB Session Start Output",
			Command:     "slb session start",
			Description: "Newly created agent session identity including its session key.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Started session credentials and metadata.",
					map[string]*output.JSONSchema{
						"session_id":   output.StringSchema("Unique identifier of the new session."),
						"session_key":  output.StringSchema("Secret HMAC signing key for this session."),
						"agent_name":   output.StringSchema("Registered agent name."),
						"program":      output.StringSchema("Client program name."),
						"model":        output.StringSchema("Reported model identifier."),
						"project_path": output.StringSchema("Project root path bound to this session."),
						"started_at":   output.DateTimeSchema("RFC3339 timestamp when the session started."),
					},
					"session_id", "session_key", "agent_name", "program", "model", "project_path", "started_at",
				)
			},
		},
		{
			Key:         "session end",
			Slug:        "session-end",
			Title:       "SLB Session End Output",
			Command:     "slb session end",
			Description: "Confirmation payload returned when ending an active session.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Session termination receipt.",
					map[string]*output.JSONSchema{
						"session_id": output.StringSchema("Identifier of the ended session."),
						"ended_at":   output.DateTimeSchema("RFC3339 timestamp when the session ended."),
					},
					"session_id", "ended_at",
				)
			},
		},
		{
			Key:         "session resume",
			Slug:        "session-resume",
			Title:       "SLB Session Resume Output",
			Command:     "slb session resume",
			Description: "Resumed or rotated session credentials and activity timestamps.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Resumed session credentials and metadata.",
					map[string]*output.JSONSchema{
						"session_id":     output.StringSchema("Active session identifier (existing or newly rotated)."),
						"session_key":    output.StringSchema("HMAC signing key for the active session."),
						"agent_name":     output.StringSchema("Agent name."),
						"program":        output.StringSchema("Client program name."),
						"model":          output.StringSchema("Reported model identifier."),
						"project_path":   output.StringSchema("Project root path bound to the session."),
						"started_at":     output.DateTimeSchema("RFC3339 timestamp when the session started."),
						"last_active_at": output.DateTimeSchema("RFC3339 timestamp of the last recorded activity."),
					},
					"session_id", "session_key", "agent_name", "program", "model", "project_path", "started_at", "last_active_at",
				)
			},
		},
		{
			Key:         "session list",
			Slug:        "session-list",
			Title:       "SLB Session List Output",
			Command:     "slb session list",
			Aliases:     []string{"session"},
			Description: "Array of active agent sessions in the selected project.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ArraySchema("List of active agent sessions.", sessionViewSchema())
			},
		},
		{
			Key:         "session heartbeat",
			Slug:        "session-heartbeat",
			Title:       "SLB Session Heartbeat Output",
			Command:     "slb session heartbeat",
			Description: "Updated activity timestamp for an active session.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Session heartbeat acknowledgment.",
					map[string]*output.JSONSchema{
						"session_id":     output.StringSchema("Session identifier updated."),
						"last_active_at": output.DateTimeSchema("RFC3339 timestamp recorded as last activity."),
					},
					"session_id", "last_active_at",
				)
			},
		},
		{
			Key:         "session reset-limits",
			Slug:        "session-reset-limits",
			Title:       "SLB Session Reset Limits Output",
			Command:     "slb session reset-limits",
			Description: "Confirmation that per-session rate-limit counters were reset.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Rate-limit reset confirmation.",
					map[string]*output.JSONSchema{
						"session_id":          output.StringSchema("Session identifier whose rate limits were reset."),
						"rate_limit_reset_at": output.DateTimeSchema("RFC3339 timestamp when rate limits were reset."),
						"status":              {Type: "string", Const: "ok", Description: "Operation status."},
					},
					"session_id", "rate_limit_reset_at", "status",
				)
			},
		},
		{
			Key:         "session gc",
			Slug:        "session-gc",
			Title:       "SLB Session GC Output",
			Command:     "slb session gc",
			Description: "Summary of stale sessions found and (unless --dry-run) ended by garbage collection.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Session garbage collection report.",
					map[string]*output.JSONSchema{
						"project_path":        output.StringSchema("Project root path scanned for stale sessions."),
						"dry_run":             output.BoolSchema("True when run in dry-run preview mode without ending sessions."),
						"threshold":           output.StringSchema("Human-readable staleness threshold duration."),
						"threshold_seconds":   output.IntSchema("Staleness threshold in whole seconds."),
						"found":               output.IntSchema("Number of stale sessions matching the threshold."),
						"ended":               output.IntSchema("Number of stale sessions ended (present when dry_run is false)."),
						"skipped":             output.IntSchema("Number of stale sessions skipped (present when dry_run is false)."),
						"ended_session_ids":   output.NullableArraySchema("Session IDs ended.", output.StringSchema("Ended session ID.")),
						"skipped_session_ids": output.NullableArraySchema("Session IDs skipped.", output.StringSchema("Skipped session ID.")),
						"sessions":            output.ArraySchema("Details of matching stale sessions.", sessionViewSchema()),
					},
					"project_path", "dry_run", "threshold", "threshold_seconds", "found", "sessions",
				)
			},
		},
		{
			Key:         "daemon start",
			Slug:        "daemon-start",
			Title:       "SLB Daemon Start Output",
			Command:     "slb daemon start",
			Description: "Process ID and IPC socket path reported when starting the approval daemon.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Daemon startup receipt.",
					map[string]*output.JSONSchema{
						"pid":         output.IntSchema("Process ID of the running daemon."),
						"socket_path": output.StringSchema("Unix domain IPC socket path."),
						"started_at":  output.DateTimeSchema("RFC3339 timestamp when daemon startup was initiated."),
						"foreground":  output.BoolSchema("True when running in foreground mode."),
					},
					"pid", "socket_path", "started_at", "foreground",
				)
			},
		},
		{
			Key:         "daemon stop",
			Slug:        "daemon-stop",
			Title:       "SLB Daemon Stop Output",
			Command:     "slb daemon stop",
			Description: "Confirmation timestamp returned when stopping the project daemon.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Daemon shutdown receipt.",
					map[string]*output.JSONSchema{
						"stopped_at": output.DateTimeSchema("RFC3339 timestamp when the daemon stopped."),
					},
					"stopped_at",
				)
			},
		},
		{
			Key:         "daemon status",
			Slug:        "daemon-status",
			Title:       "SLB Daemon Status Output",
			Command:     "slb daemon status",
			Aliases:     []string{"daemon"},
			Description: "Runtime health, PID, socket liveness, uptime, and project queue metrics for the approval daemon.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Daemon liveness and queue status snapshot.",
					map[string]*output.JSONSchema{
						"running":         output.BoolSchema("True when the project daemon is running and responsive."),
						"status":          output.StringSchema("Daemon lifecycle status string."),
						"pid":             output.IntSchema("Recorded daemon PID, or 0 when not running."),
						"pid_file":        output.StringSchema("Path to the daemon PID file."),
						"uptime":          output.StringSchema("Formatted daemon uptime duration."),
						"uptime_seconds":  output.IntSchema("Daemon uptime in whole seconds."),
						"pending_count":   output.IntSchema("Number of pending requests in the project database."),
						"active_sessions": output.IntSchema("Number of active agent sessions in the project database."),
						"socket_path":     output.StringSchema("Unix domain IPC socket path."),
						"socket_alive":    output.BoolSchema("True when the IPC socket responds to ping."),
						"message":         output.StringSchema("Human-readable diagnostic summary."),
					},
					"running", "status", "pid", "pid_file", "uptime", "uptime_seconds",
					"pending_count", "active_sessions", "socket_path", "socket_alive", "message",
				)
			},
		},
		{
			Key:         "daemon logs",
			Slug:        "daemon-logs",
			Title:       "SLB Daemon Logs Output",
			Command:     "slb daemon logs",
			Description: "Log file path and tailed log lines returned by slb daemon logs.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Daemon log tail payload.",
					map[string]*output.JSONSchema{
						"log_path": output.StringSchema("Filesystem path to the daemon log file."),
						"lines":    output.NullableArraySchema("Tailed log lines from the daemon log.", output.StringSchema("Single log line.")),
					},
					"log_path", "lines",
				)
			},
		},
		{
			Key:         "audit query",
			Slug:        "audit-query",
			Title:       "SLB Audit Query Output",
			Command:     "slb audit query",
			Aliases:     []string{"audit"},
			Description: "Array of blocked/asked hook audit events matching filter criteria (or single Event per line with --jsonl).",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				item := output.SchemaFromType(reflect.TypeOf(audit.Event{}), map[string]string{
					"version":          "Audit event schema version.",
					"id":               "Unique audit event identifier.",
					"observation_id":   "Advisory correlation ID linking daemon and fallback observations.",
					"timestamp":        "RFC3339 timestamp when the hook decision occurred.",
					"command_redacted": "Redacted command text evaluated by the hook.",
					"command_hash":     "SHA-256 hash of the evaluated command.",
					"cwd":              "Working directory where the command was evaluated.",
					"session_id":       "Hook session identifier when supplied.",
					"action":           "Hook enforcement action (block or ask).",
					"tier":             "Matched risk tier.",
					"matched_pattern":  "Pattern or rule that triggered the hook decision.",
					"min_approvals":    "Minimum approvals required for the matched tier.",
					"source":           "Evaluation source (daemon or local fallback).",
					"approval_policy":  "Approval policy active at evaluation time.",
					"review_notice":    "Human-readable approval policy notice.",
					"socket_path":      "Daemon IPC socket path contacted.",
					"daemon_error":     "Daemon query error when fallback was used.",
				})
				return output.ArraySchema("List of hook audit events.", item)
			},
		},
		{
			Key:         "audit prune",
			Slug:        "audit-prune",
			Title:       "SLB Audit Prune Output",
			Command:     "slb audit prune",
			Description: "Summary of audit records matched or removed prior to the cutoff timestamp.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Audit log pruning summary.",
					map[string]*output.JSONSchema{
						"before":  output.DateTimeSchema("Exclusive RFC3339 cutoff timestamp."),
						"dry_run": output.BoolSchema("True when records were only counted without deletion."),
						"count":   output.IntSchema("Number of audit records matched or removed."),
					},
					"before", "dry_run", "count",
				)
			},
		},
		{
			Key:         "audit emergency",
			Slug:        "audit-emergency",
			Title:       "SLB Emergency Audit Output",
			Command:     "slb audit emergency",
			Description: "Array of unreviewed emergency execution ledger records.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				item := output.SchemaFromType(reflect.TypeOf(emergencyAuditView{}), map[string]string{
					"configured_approval_policy":       "Project approval policy that was bypassed.",
					"configured_approval_policy_label": "Notice marking the configured policy as bypassed.",
					"approval_policy_bypassed":         "Always true for emergency audit records.",
					"emergency_id":                     "Unique emergency execution record identifier.",
					"execution_mode":                   "Always 'emergency'.",
					"assurance":                        "Always 'emergency_unreviewed'.",
					"review_notice":                    "Notice stating this execution bypassed review.",
					"project_path":                     "Project root path owning the emergency record.",
					"command":                          "Redacted command text.",
					"command_hash":                     "SHA-256 hash of normalized command.",
					"hash":                             "SHA-256 hash of raw command.",
					"cwd":                              "Working directory where emergency command ran.",
					"actor":                            "Unauthenticated local actor string recorded.",
					"actor_authenticated":              "Always false because local actor labels are unauthenticated.",
					"reason":                           "Redacted reason recorded for emergency override.",
					"confirmation_method":              "Mechanism used to confirm emergency execution.",
					"status":                           "Recorded emergency execution lifecycle status.",
					"recorded_at":                      "RFC3339 timestamp when emergency intent was recorded.",
					"started_at":                       "RFC3339 timestamp when the process started.",
					"completed_at":                     "RFC3339 timestamp when the process completed.",
					"pid":                              "Process ID of the emergency command.",
					"exit_code":                        "Command exit code, or null if not completed.",
					"duration_ms":                      "Elapsed duration in milliseconds, or null.",
					"log_path":                         "Path to the emergency execution log file.",
					"rollback_path":                    "Path to captured rollback state, if any.",
					"error":                            "Redacted error message when execution failed.",
				})
				return output.ArraySchema("List of emergency execution audit records.", item)
			},
		},
		{
			Key:         "outcome record",
			Slug:        "outcome-record",
			Title:       "SLB Outcome Record Output",
			Command:     "slb outcome record",
			Description: "Recorded post-execution outcome feedback for an executed request.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Recorded execution outcome payload.",
					withPolicy(map[string]*output.JSONSchema{
						"id":                  output.IntSchema("Unique numeric outcome record identifier."),
						"request_id":          output.StringSchema("Executed request identifier."),
						"caused_problems":     output.BoolSchema("True if the executed command caused problems."),
						"problem_description": output.StringSchema("Description of problems caused, if any."),
						"human_rating":        output.NullableIntSchema("Optional human rating (1-5), or null."),
						"human_notes":         output.StringSchema("Optional human feedback notes."),
						"recorded_at":         output.DateTimeSchema("RFC3339 timestamp when the outcome was recorded."),
						"assurance":           output.EnumSchema("Execution evidence assurance level.", reviewAssuranceValues...),
					}),
					"approval_policy", "approval_policy_label", "id", "request_id",
					"caused_problems", "problem_description", "human_rating", "human_notes", "recorded_at",
				)
			},
		},
		{
			Key:         "outcome list",
			Slug:        "outcome-list",
			Title:       "SLB Outcome List Output",
			Command:     "slb outcome list",
			Aliases:     []string{"outcome"},
			Description: "List of recorded execution outcomes with total count.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				item := output.ObjectSchema(
					"Single recorded execution outcome.",
					withPolicy(map[string]*output.JSONSchema{
						"id":                  output.IntSchema("Unique numeric outcome record identifier."),
						"request_id":          output.StringSchema("Executed request identifier."),
						"caused_problems":     output.BoolSchema("True if execution caused problems."),
						"created_at":          output.DateTimeSchema("RFC3339 timestamp when outcome was recorded."),
						"assurance":           output.EnumSchema("Execution evidence assurance level.", reviewAssuranceValues...),
						"problem_description": output.StringSchema("Description of problems caused."),
						"human_rating":        output.IntSchema("Human rating (1-5)."),
						"human_notes":         output.StringSchema("Additional human notes."),
					}),
					"approval_policy", "approval_policy_label", "id", "request_id", "caused_problems", "created_at",
				)
				return output.ObjectSchema(
					"Execution outcomes list wrapper.",
					map[string]*output.JSONSchema{
						"outcomes": output.ArraySchema("Matching execution outcome records.", item),
						"count":    output.IntSchema("Number of outcome records returned."),
					},
					"outcomes", "count",
				)
			},
		},
		{
			Key:         "outcome stats",
			Slug:        "outcome-stats",
			Title:       "SLB Outcome Stats Output",
			Command:     "slb outcome stats",
			Description: "Aggregate execution outcome and time-to-approval metrics.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				outcomesObj := output.ObjectSchema(
					"Aggregate execution outcome statistics.",
					map[string]*output.JSONSchema{
						"total":               output.IntSchema("Total recorded outcomes."),
						"problematic_count":   output.IntSchema("Number of outcomes marked problematic."),
						"problematic_percent": output.NumberSchema("Percentage of outcomes marked problematic."),
						"rated_count":         output.IntSchema("Number of outcomes with a human rating."),
						"avg_rating":          output.NumberSchema("Average human rating across rated outcomes."),
					},
					"total", "problematic_count", "problematic_percent", "rated_count", "avg_rating",
				)
				timesObj := output.ObjectSchema(
					"Aggregate time-to-approval statistics in minutes.",
					map[string]*output.JSONSchema{
						"sample_size":    output.IntSchema("Number of approved requests in the sample."),
						"avg_minutes":    output.NumberSchema("Mean time to approval in minutes."),
						"median_minutes": output.NumberSchema("Median time to approval in minutes."),
						"min_minutes":    output.NumberSchema("Minimum time to approval in minutes."),
						"max_minutes":    output.NumberSchema("Maximum time to approval in minutes."),
					},
					"sample_size", "avg_minutes", "median_minutes", "min_minutes", "max_minutes",
				)
				return output.ObjectSchema(
					"Aggregate outcome and approval timing metrics.",
					map[string]*output.JSONSchema{
						"outcomes":       outcomesObj,
						"approval_times": timesObj,
					},
					"outcomes", "approval_times",
				)
			},
		},
		{
			Key:         "outcome agent-stats",
			Slug:        "outcome-agent-stats",
			Title:       "SLB Outcome Agent Stats Output",
			Command:     "slb outcome agent-stats",
			Description: "Per-agent request approval, rejection, execution, and problem statistics.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Request and outcome metrics for a specific agent.",
					map[string]*output.JSONSchema{
						"agent_name":      output.StringSchema("Agent name queried."),
						"total_requests":  output.IntSchema("Total requests submitted by this agent."),
						"approved_count":  output.IntSchema("Number of requests approved."),
						"rejected_count":  output.IntSchema("Number of requests rejected."),
						"executed_count":  output.IntSchema("Number of requests executed."),
						"problematic_pct": output.NumberSchema("Percentage of executed outcomes marked problematic."),
					},
					"agent_name", "total_requests", "approved_count", "rejected_count", "executed_count", "problematic_pct",
				)
			},
		},
		{
			Key:         "hook generate",
			Slug:        "hook-generate",
			Title:       "SLB Hook Generate Output",
			Command:     "slb hook generate",
			Description: "Metadata for a generated standalone Claude Code Python guard script.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Generated hook script metadata.",
					map[string]*output.JSONSchema{
						"status":          {Type: "string", Const: "generated", Description: "Generation completion status."},
						"script_path":     output.StringSchema("Path where the hook script was written."),
						"pattern_hash":    output.StringSchema("SHA-256 hash of embedded classification patterns."),
						"pattern_count":   output.IntSchema("Total number of embedded patterns."),
						"approval_policy": output.EnumSchema("Approval policy embedded in the generated hook.", approvalPolicyValues...),
						"review_notice":   output.StringSchema("Human-readable approval policy notice."),
						"policy_config":   output.StringSchema("Selected explicit policy config path, if any."),
						"policy_project":  output.StringSchema("Project root path associated with the policy."),
					},
					"status", "script_path", "pattern_hash", "pattern_count",
					"approval_policy", "review_notice", "policy_config", "policy_project",
				)
			},
		},
		{
			Key:         "hook install",
			Slug:        "hook-install",
			Title:       "SLB Hook Install Output",
			Command:     "slb hook install",
			Description: "Result of installing SLB's Claude Code PreToolUse hook registration.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return &output.JSONSchema{
					Type:        "object",
					Description: "Hook installation status and registered settings metadata.",
					Properties: map[string]*output.JSONSchema{
						"status":                      output.StringSchema("Installation result status (installed)."),
						"scope":                       output.EnumSchema("Installation scope.", "project", "global"),
						"settings_path":               output.StringSchema("Claude Code settings.json path updated."),
						"settings_changed":            output.BoolSchema("True when settings.json content changed."),
						"hook_script":                 output.StringSchema("Fallback hook script path."),
						"hook_command":                output.StringSchema("Installed PreToolUse command string."),
						"native_guard":                output.BoolSchema("True when native 'slb hook guard' is registered."),
						"on_failure":                  output.StringSchema("Configured hook failure mode (block)."),
						"upgraded":                    output.BoolSchema("True if an existing SLB hook entry was upgraded."),
						"already_existed":             output.BoolSchema("True if an identical SLB hook entry already existed."),
						"matcher":                     output.StringSchema("Tool matcher configured in Claude settings."),
						"sibling_hooks_preserved":     output.IntSchema("Count of non-SLB hooks preserved in settings.json."),
						"approval_policy":             output.EnumSchema("Effective approval policy bound to the hook.", approvalPolicyValues...),
						"review_notice":               output.StringSchema("Human-readable approval policy notice."),
						"policy_config":               output.StringSchema("Bound explicit policy config path, if any."),
						"policy_project":              output.StringSchema("Bound project root path."),
						"previous_command":            output.StringSchema("Previously registered SLB hook command when upgraded."),
						"duplicate_slb_hooks_removed": output.IntSchema("Count of duplicate SLB hook entries removed during installation."),
					},
					Required:             []string{"approval_policy", "hook_command", "native_guard", "on_failure", "review_notice", "scope", "settings_changed", "settings_path", "status"},
					AdditionalProperties: true,
				}
			},
		},
		{
			Key:         "hook uninstall",
			Slug:        "hook-uninstall",
			Title:       "SLB Hook Uninstall Output",
			Command:     "slb hook uninstall",
			Description: "Result of removing SLB's Claude Code hook registration from project or global settings.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.ObjectSchema(
					"Hook uninstall result.",
					map[string]*output.JSONSchema{
						"status":        output.EnumSchema("Uninstall status.", "uninstalled", "not_installed"),
						"scope":         output.EnumSchema("Target scope.", "project", "global"),
						"settings_path": output.StringSchema("Claude Code settings.json path inspected."),
						"removed":       output.BoolSchema("True when one or more SLB hook entries were removed."),
						"removed_count": output.IntSchema("Count of SLB hook entries removed."),
						"message":       output.StringSchema("Diagnostic message when no SLB hook was installed."),
					},
					"status", "scope", "settings_path",
				)
			},
		},
		{
			Key:         "hook status",
			Slug:        "hook-status",
			Title:       "SLB Hook Status Output",
			Command:     "slb hook status",
			Aliases:     []string{"hook"},
			Description: "Installation, drift, pattern-hash, and daemon-liveness status for the Claude Code hook.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return &output.JSONSchema{
					Type:        "object",
					Description: "Hook installation and daemon synchronization status.",
					Properties: map[string]*output.JSONSchema{
						"status":                        output.EnumSchema("Overall hook status.", "installed", "stale", "degraded", "partial", "not_installed"),
						"scope":                         output.EnumSchema("Inspected scope.", "project", "global"),
						"hook_script_exists":            output.BoolSchema("True when the fallback Python hook script exists."),
						"hook_script_path":              output.StringSchema("Path to the fallback Python hook script."),
						"settings_configured":           output.BoolSchema("True when Claude settings.json registers SLB."),
						"settings_path":                 output.StringSchema("Path to Claude Code settings.json."),
						"failure_policy_configured":     output.BoolSchema("True when fail-closed on_failure=block is configured."),
						"current_pattern_hash":          output.StringSchema("SHA-256 hash of current effective patterns."),
						"installed_pattern_hash":        output.StringSchema("SHA-256 hash embedded in installed hook."),
						"pattern_hash_matches":          output.BoolSchema("True when installed pattern hash matches current."),
						"current_hook_caution_action":   output.StringSchema("Current effective CAUTION hook action."),
						"installed_hook_caution_action": output.StringSchema("Installed CAUTION hook action."),
						"hook_caution_action_matches":   output.BoolSchema("True when CAUTION actions match."),
						"daemon_reachable":              output.BoolSchema("True when project daemon responded to health probe."),
						"daemon_status":                 output.StringSchema("Reported daemon health status."),
						"daemon_pattern_hash":           output.StringSchema("Pattern hash reported by the daemon."),
						"daemon_pattern_hash_matches":   output.BoolSchema("True when daemon pattern hash matches current."),
						"current_approval_policy":       output.EnumSchema("Current effective approval policy.", approvalPolicyValues...),
						"current_review_notice":         output.StringSchema("Current approval policy notice."),
						"installed_approval_policy":     output.StringSchema("Approval policy embedded in installed hook."),
						"approval_policy_matches":       output.BoolSchema("True when approval policies match."),
					},
					Required:             []string{"status", "scope", "hook_script_exists", "settings_configured", "current_pattern_hash", "daemon_reachable"},
					AdditionalProperties: true,
				}
			},
		},
		{
			Key:         "hook health",
			Slug:        "hook-health",
			Title:       "SLB Hook Health Output",
			Command:     "slb hook health",
			Description: "Live daemon hook-query health and policy parity diagnostic.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return &output.JSONSchema{
					Type:        "object",
					Description: "Hook daemon reachability and policy parity check.",
					Properties: map[string]*output.JSONSchema{
						"status":                      output.StringSchema("Daemon health status (ok, unreachable, or degraded)."),
						"healthy":                     output.BoolSchema("True when daemon is reachable and policy/hashes match."),
						"daemon_reachable":            output.BoolSchema("True when the daemon responded within timeout."),
						"fallback_available":          output.BoolSchema("True indicating local fail-closed fallback is available."),
						"current_pattern_hash":        output.StringSchema("SHA-256 hash of current local patterns."),
						"current_hook_caution_action": output.StringSchema("Current local CAUTION hook action."),
						"cwd":                         output.StringSchema("Working directory evaluated."),
						"current_approval_policy":     output.EnumSchema("Current local approval policy.", approvalPolicyValues...),
						"current_review_notice":       output.StringSchema("Human-readable notice for current local policy."),
						"current_policy_config":       output.StringSchema("Selected explicit config path, if any."),
						"current_policy_project":      output.StringSchema("Project root path supplying local policy."),
						"error":                       output.StringSchema("Diagnostic error when daemon was unreachable."),
					},
					Required:             []string{"status", "healthy", "daemon_reachable", "fallback_available", "current_pattern_hash", "cwd"},
					AdditionalProperties: true,
				}
			},
		},
		{
			Key:         "hook test",
			Slug:        "hook-test",
			Title:       "SLB Hook Test Output",
			Command:     "slb hook test",
			Description: "Simulated or live hook guard decision for a command string.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return &output.JSONSchema{
					Type:        "object",
					Description: "Hook evaluation decision for a test command.",
					Properties: map[string]*output.JSONSchema{
						"command":         output.StringSchema("Command string evaluated."),
						"action":          output.EnumSchema("Hook action decision.", "allow", "block", "ask", "execute"),
						"message":         output.StringSchema("Human-readable decision explanation."),
						"tier":            output.StringSchema("Matched risk tier."),
						"matched_pattern": output.StringSchema("Pattern or rule that triggered the decision."),
						"min_approvals":   output.IntSchema("Minimum approvals required."),
						"needs_approval":  output.BoolSchema("True when approval is required."),
						"request_id":      output.StringSchema("Approved request ID matched for execution handoff, if any."),
						"source":          output.EnumSchema("Decision source.", "daemon", "local"),
						"fallback":        output.BoolSchema("True when local fallback was used after daemon failure."),
						"local_only":      output.BoolSchema("True when --local-only bypassed daemon query."),
						"approval_policy": output.EnumSchema("Effective approval policy.", approvalPolicyValues...),
						"review_notice":   output.StringSchema("Human-readable approval policy notice."),
						"policy_config":   output.StringSchema("Selected explicit config path, if any."),
						"policy_project":  output.StringSchema("Project root path supplying policy."),
					},
					Required:             []string{"command", "action", "message", "source", "fallback", "approval_policy", "review_notice"},
					AdditionalProperties: true,
				}
			},
		},
		{
			Key:         "git-check",
			Slug:        "git-check",
			Title:       "SLB Native Git Check Output",
			Command:     "slb git-check",
			Description: "Native Git hook risk assessment (default) or approval decision payload (with --request).",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				desc := gitAssessmentFieldDescriptions()
				desc["approval_policy"] = "Effective approval policy governing the Git operation."
				desc["approval_policy_label"] = "Human-readable approval policy notice."
				desc["assurance"] = "Recorded assurance level when authorized."
				desc["allowed"] = "True when the Git operation is allowed to proceed without blocking."
				desc["request_id"] = "Matching approved request ID, if any."
				desc["status"] = "Request status if matched."
				desc["message"] = "Human-readable explanation of the Git hook decision."
				desc["assessment"] = "Structured Git operation assessment."
				decision := output.SchemaFromType(reflect.TypeOf(gitHookDecision{}), desc)
				return &output.JSONSchema{
					Description: "Either a GitAssessment (default) or a gitHookDecision (with --request).",
					AnyOf:       []*output.JSONSchema{gitAssessmentSchema(), decision},
				}
			},
		},
		{
			Key:         "git-hooks status",
			Slug:        "git-hooks-status",
			Title:       "SLB Native Git Hooks Status Output",
			Command:     "slb git-hooks status",
			Description: "Installation and management status of SLB native Git hooks.",
			Formats:     standardFormats,
			Build:       gitHookStatusListSchema,
		},
		{
			Key:         "git-hooks install",
			Slug:        "git-hooks-install",
			Title:       "SLB Native Git Hooks Install Output",
			Command:     "slb git-hooks install",
			Description: "Resulting hook statuses after installing SLB native Git hooks.",
			Formats:     standardFormats,
			Build:       gitHookStatusListSchema,
		},
		{
			Key:         "git-hooks uninstall",
			Slug:        "git-hooks-uninstall",
			Title:       "SLB Native Git Hooks Uninstall Output",
			Command:     "slb git-hooks uninstall",
			Description: "Resulting hook statuses after uninstalling SLB native Git hooks.",
			Formats:     standardFormats,
			Build:       gitHookStatusListSchema,
		},
		{
			Key:         "rollback",
			Slug:        "rollback",
			Title:       "SLB Rollback Output",
			Command:     "slb rollback",
			Description: "Result of request rollback, Git snapshot listing (--list), or Git snapshot recovery (--from).",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				reqRollback := output.ObjectSchema(
					"Request rollback completion receipt.",
					map[string]*output.JSONSchema{
						"request_id":     output.StringSchema("Identifier of the rolled-back request."),
						"rollback_path":  output.StringSchema("Filesystem path to the rollback capture directory."),
						"rolled_back_at": output.DateTimeSchema("RFC3339 timestamp when rollback completed."),
						"status":         {Type: "string", Const: "rolled_back", Description: "Rollback completion status."},
						"message":        output.StringSchema("Human-readable rollback summary."),
					},
					"request_id", "rollback_path", "rolled_back_at", "status", "message",
				)
				snapList := output.ArraySchema(
					"List of available Git rollback snapshots (--list).",
					output.SchemaFromType(reflect.TypeOf(core.GitRollbackSnapshot{}), map[string]string{
						"snapshot_path": "Filesystem path to the rollback snapshot.",
						"request_id":    "Associated SLB request ID.",
						"captured_at":   "RFC3339 timestamp when the snapshot was captured.",
						"project_path":  "Project root path owning the snapshot.",
						"error":         "Diagnostic error if snapshot metadata could not be read.",
					}),
				)
				recovery := output.SchemaFromType(reflect.TypeOf(core.GitRollbackRecoveryResult{}), map[string]string{
					"request_id":       "Associated SLB request ID.",
					"rollback_path":    "Filesystem path to the restored snapshot.",
					"rolled_back_at":   "RFC3339 timestamp when recovery completed.",
					"status":           "Recovery completion status.",
					"recovery_mode":    "Git recovery mechanism used.",
					"database_updated": "True if the project database recorded the rollback timestamp.",
					"receipt_path":     "Path to the persisted recovery receipt.",
					"receipt_error":    "Diagnostic error if writing the recovery receipt failed.",
				})
				return &output.JSONSchema{
					Description: "Rollback receipt, snapshot list, or Git recovery result.",
					AnyOf:       []*output.JSONSchema{reqRollback, snapList, recovery},
				}
			},
		},
		{
			Key:         "emergency-execute",
			Slug:        "emergency-execute",
			Title:       "SLB Emergency Execute Output",
			Command:     "slb emergency-execute",
			Description: "Unreviewed emergency override execution outcome recorded in the emergency ledger.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.SchemaFromType(reflect.TypeOf(emergencyExecutionOutcome{}), map[string]string{
					"approval_policy":          "Project approval policy bypassed by emergency execution.",
					"approval_policy_label":    "Notice marking the configured policy as bypassed.",
					"approval_policy_bypassed": "Always true for emergency execution.",
					"execution_mode":           "Always 'emergency'.",
					"assurance":                "Always 'emergency_unreviewed'.",
					"review_notice":            "Notice stating execution bypassed review.",
					"emergency_id":             "Unique emergency execution identifier.",
					"command":                  "Command executed under emergency override.",
					"hash":                     "SHA-256 hash of raw command.",
					"command_hash":             "SHA-256 hash of normalized command.",
					"project_path":             "Project root path.",
					"cwd":                      "Working directory where command ran.",
					"confirmation_method":      "Mechanism used to confirm the override.",
					"status":                   "Durable emergency execution status.",
					"executed":                 "True when the command process started.",
					"exit_code":                "Command exit code, or -1 if not executed.",
					"duration_ms":              "Elapsed duration in milliseconds.",
					"log_path":                 "Path to captured execution log.",
					"rollback_path":            "Path to captured rollback snapshot, if any.",
					"output":                   "Captured command output.",
					"timed_out":                "True when execution timed out.",
					"reason":                   "Recorded reason for emergency override.",
					"actor":                    "Unauthenticated local actor string.",
					"attempted_at":             "RFC3339Nano timestamp when attempt was recorded.",
					"executed_at":              "RFC3339Nano timestamp when execution started.",
					"recorded_at":              "Timestamp when emergency intent was recorded.",
					"started_at":               "Timestamp when process started.",
					"completed_at":             "Timestamp when process completed.",
					"pid":                      "Process ID of the executed command.",
					"error":                    "Error message if execution failed.",
				})
			},
		},
		{
			Key:         "error",
			Slug:        "error",
			Title:       "SLB Structured Error Output",
			Command:     "slb (error)",
			Description: "Canonical structured error payload emitted in JSON/YAML/TOON modes.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				return output.SchemaFromType(reflect.TypeOf(output.ErrorPayload{}), map[string]string{
					"error":   "Machine-readable error code or category.",
					"message": "Human-readable error description.",
					"details": "Optional structured error context (such as numeric exit code).",
				})
			},
		},
		{
			Key:         "schema",
			Slug:        "schema",
			Title:       "SLB Schema Catalog Output",
			Command:     "slb schema",
			Aliases:     []string{"schema list"},
			Description: "Catalog of available JSON Schemas emitted when running slb schema without arguments.",
			Formats:     standardFormats,
			Build: func() *output.JSONSchema {
				item := output.ObjectSchema(
					"Metadata entry for one registered command output schema.",
					map[string]*output.JSONSchema{
						"command":     output.StringSchema("CLI command invocation described by this schema."),
						"key":         output.StringSchema("Canonical lookup key for 'slb schema <key>'."),
						"$id":         output.StringSchema("Canonical JSON Schema $id URI."),
						"title":       output.StringSchema("Human-readable schema title."),
						"description": output.StringSchema("Summary of the command output structure."),
						"aliases":     output.ArraySchema("Alternate lookup aliases accepted by slb schema.", output.StringSchema("Alias string.")),
						"streaming":   output.BoolSchema("True when the command emits newline-delimited JSON records."),
						"formats":     output.ArraySchema("Structured output formats supported by this command.", output.StringSchema("Format name.")),
					},
					"command", "key", "$id", "title", "description", "formats",
				)
				return output.ObjectSchema(
					"Catalog listing all registered SLB output JSON Schemas.",
					map[string]*output.JSONSchema{
						"schema_draft": output.StringSchema("JSON Schema specification URI implemented."),
						"count":        output.IntSchema("Number of registered schemas."),
						"schemas":      output.ArraySchema("Registered command output schemas.", item),
					},
					"schema_draft", "count", "schemas",
				)
			},
		},
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Key < entries[j].Key
	})
	return entries
}
