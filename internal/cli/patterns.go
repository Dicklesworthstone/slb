package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagPatternTier       string
	flagPatternReason     string
	flagPatternExitCode   bool
	flagPatternFormat     string
	flagPatternOutputFile string
)

// loadCustomPatternsIntoDefaultEngine merges every row in the project's
// `custom_patterns` table into the global pattern engine. Without this,
// `slb patterns add` persists the row to SQLite but a fresh CLI process
// (e.g. `slb patterns test`) only ever sees the builtin patterns
// because the engine is initialized at package load with builtins only.
//
// Best-effort: if the database can't be opened, returns nil so commands
// that don't strictly need custom patterns (e.g. running before
// `slb init`) still work against builtins. Returns the number of
// patterns loaded.
func loadCustomPatternsIntoDefaultEngine() (int, error) {
	dbConn, err := db.OpenAndMigrate(GetDB())
	if err != nil {
		// No project DB yet: silently fall back to builtins-only.
		// This matches the behavior of `slb classify` etc. on a
		// freshly-installed system before `slb init`.
		return 0, nil
	}
	defer dbConn.Close()

	rows, err := dbConn.ListCustomPatterns()
	if err != nil {
		return 0, fmt.Errorf("loading custom patterns: %w", err)
	}

	engine := core.GetDefaultEngine()
	// Snapshot existing engine patterns by (tier, pattern) so this
	// helper is idempotent across calls in the same process. Without
	// dedup, calling loadCustomPatternsIntoDefaultEngine twice (e.g.
	// in a test that creates two cobra command trees, or any future
	// long-running mode) would append the same row twice and the
	// in-memory engine would diverge from the SQLite source of truth.
	existing := make(map[string]struct{})
	for tierName, list := range engine.AllPatterns() {
		for _, p := range list {
			existing[tierName+"\x00"+p.Pattern] = struct{}{}
		}
	}

	loaded := 0
	for _, row := range rows {
		tier := parseTier(row.Tier)
		if tier == "" {
			// Unknown tier — persisted by an older CLI version or
			// edited directly in SQL. Skipping is safer than
			// blindly routing it to the safe bucket (which is
			// what engine.AddPattern's default arm would do).
			fmt.Fprintf(os.Stderr,
				"warning: skipping persisted pattern with unrecognized tier %q (pattern=%q)\n",
				row.Tier, row.Pattern)
			continue
		}
		// Use the canonical lowercase tier name (from parseTier) as
		// the dedup key. If a row was inserted with mixed-case tier
		// (e.g. via direct SQL edits), it would still match the
		// engine's lowercase keys.
		key := string(tier) + "\x00" + row.Pattern
		if _, dup := existing[key]; dup {
			continue
		}
		if err := engine.AddPattern(tier, row.Pattern, row.Description, row.Source); err != nil {
			// A persisted pattern that won't compile is a real
			// problem, but shouldn't take down the whole CLI —
			// log and continue so other patterns still load.
			fmt.Fprintf(os.Stderr, "warning: skipping invalid persisted pattern %q (tier=%s): %v\n",
				row.Pattern, row.Tier, err)
			continue
		}
		// Mark loaded so that two duplicate rows in the same DB
		// table (e.g. inserted before the UNIQUE constraint was
		// added) don't both populate the engine.
		existing[key] = struct{}{}
		loaded++
	}
	return loaded, nil
}

func init() {
	// patterns command
	patternsCmd.PersistentFlags().StringVarP(&flagPatternTier, "tier", "T", "", "risk tier (critical, dangerous, caution, safe)")
	patternsCmd.PersistentFlags().StringVarP(&flagPatternReason, "reason", "r", "", "reason for adding/removing pattern")

	// patterns test/check flags
	patternsTestCmd.Flags().BoolVar(&flagPatternExitCode, "exit-code", false, "return non-zero exit code if approval needed")

	// patterns export flags
	patternsExportCmd.Flags().StringVarP(&flagPatternFormat, "format", "f", "json", "export format: json, yaml, claude-hook")
	patternsExportCmd.Flags().StringVar(&flagPatternOutputFile, "output", "", "output file (default: stdout)")

	// Add subcommands
	patternsCmd.AddCommand(patternsListCmd)
	patternsCmd.AddCommand(patternsTestCmd)
	patternsCmd.AddCommand(patternsAddCmd)
	patternsCmd.AddCommand(patternsRemoveCmd)
	patternsCmd.AddCommand(patternsRequestRemovalCmd)
	patternsCmd.AddCommand(patternsSuggestCmd)
	patternsCmd.AddCommand(patternsExportCmd)
	patternsCmd.AddCommand(patternsVersionCmd)

	// Add alias: slb check "<command>" is alias for slb patterns test "<command>"
	rootCmd.AddCommand(patternsCmd)
	rootCmd.AddCommand(checkCmd)
}

var patternsCmd = &cobra.Command{
	Use:   "patterns",
	Short: "Manage command classification patterns",
	Long: `Manage the patterns used to classify commands into risk tiers.

Patterns are regex strings matched against normalized commands.
Commands are classified in order: SAFE → CRITICAL → DANGEROUS → CAUTION.
The first matching pattern determines the tier.

Agents can ADD patterns freely (making things safer) but CANNOT remove patterns.
Pattern removal requires human approval through the TUI.`,
}

var patternsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all patterns grouped by tier",
	Long: `List all patterns used for command classification.

Use --tier to filter by a specific tier (safe, critical, dangerous, caution).
Without --tier, all patterns from all tiers are shown.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		engine := core.GetDefaultEngine()
		out := output.New(output.Format(GetOutput()))

		if flagPatternTier != "" {
			// Filter by tier
			tier := parseTier(flagPatternTier)
			if tier == "" && flagPatternTier != "safe" {
				return fmt.Errorf("invalid tier: %s (must be safe, critical, dangerous, or caution)", flagPatternTier)
			}
			patterns := engine.ListPatterns(tier)
			return outputPatterns(out, map[string][]*core.Pattern{flagPatternTier: patterns})
		}

		// All patterns
		all := engine.AllPatterns()
		return outputPatterns(out, all)
	},
}

var patternsTestCmd = &cobra.Command{
	Use:   "test <command>",
	Short: "Test which tier a command matches",
	Long: `Test a command against all patterns and show its risk classification.

Returns the tier, matched pattern, minimum approvals required, and whether
approval is needed.

Use --exit-code to return non-zero (exit 1) if approval is needed.
This is useful for Claude Code hooks integration.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Merge custom_patterns from the project DB on top of the
		// builtin set so `patterns test` sees what `patterns add`
		// just persisted. Best-effort — a missing DB falls back to
		// builtins-only.
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}

		command := args[0]
		cwd, _ := os.Getwd()

		result := core.Classify(command, cwd)

		// Build response
		resp := map[string]any{
			"command":        command,
			"needs_approval": result.NeedsApproval,
			"is_safe":        result.IsSafe,
			"min_approvals":  result.MinApprovals,
		}

		if result.Tier != "" {
			resp["tier"] = string(result.Tier)
		} else {
			resp["tier"] = nil
		}

		if result.MatchedPattern != "" {
			resp["matched_pattern"] = result.MatchedPattern
		}

		if result.ParseError {
			resp["parse_error"] = true
		}

		if len(result.MatchedSegments) > 0 {
			segments := make([]map[string]any, 0, len(result.MatchedSegments))
			for _, seg := range result.MatchedSegments {
				segments = append(segments, map[string]any{
					"segment":         seg.Segment,
					"tier":            string(seg.Tier),
					"matched_pattern": seg.MatchedPattern,
				})
			}
			resp["matched_segments"] = segments
		}

		// Handle output format
		format := GetOutput()
		if format == "text" {
			// Human-readable text output
			fmt.Printf("Command:    %s\n", command)
			if tier, ok := resp["tier"].(string); ok && tier != "" {
				fmt.Printf("Tier:       %s\n", strings.ToUpper(tier))
			} else {
				fmt.Printf("Tier:       (none)\n")
			}
			fmt.Printf("Safe:       %v\n", result.IsSafe)
			fmt.Printf("Approval:   %v\n", result.NeedsApproval)
			if result.NeedsApproval {
				fmt.Printf("Min Approvals: %d\n", result.MinApprovals)
			}
			if result.MatchedPattern != "" {
				fmt.Printf("Pattern:    %s\n", result.MatchedPattern)
			}
			if len(result.MatchedSegments) > 0 {
				fmt.Printf("Segments:\n")
				for _, seg := range result.MatchedSegments {
					fmt.Printf("  - %s (%s)\n", seg.Segment, seg.Tier)
				}
			}
		} else {
			out := output.New(output.Format(format))
			if err := out.Write(resp); err != nil {
				return err
			}
		}

		// Exit code handling for hooks integration
		if flagPatternExitCode && result.NeedsApproval {
			// Flush stdout before exiting
			os.Stdout.Sync()
			os.Exit(1)
		}

		return nil
	},
}

// checkCmd is an alias for "patterns test"
var checkCmd = &cobra.Command{
	Use:   "check <command>",
	Short: "Alias for 'patterns test' - test which tier a command matches",
	Long:  `Alias for 'slb patterns test'. See 'slb patterns test --help' for details.`,
	Args:  cobra.ExactArgs(1),
	RunE:  patternsTestCmd.RunE,
}

var patternsAddCmd = &cobra.Command{
	Use:   "add <pattern>",
	Short: "Add a new pattern to a tier",
	Long: `Add a new regex pattern to classify commands.

Agents CAN add patterns freely - this makes classification stricter (safer).
The --tier flag is required to specify which tier the pattern applies to.

Examples:
  slb patterns add "^dangerous-cmd" --tier dangerous --reason "Custom dangerous command"
  slb patterns add "^my-safe-script" --tier safe --reason "Known safe internal script"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := args[0]

		if flagPatternTier == "" {
			return fmt.Errorf("--tier is required (safe, critical, dangerous, or caution)")
		}

		tier := parseTier(flagPatternTier)
		if tier == "" && flagPatternTier != "safe" {
			return fmt.Errorf("invalid tier: %s", flagPatternTier)
		}

		engine := core.GetDefaultEngine()
		if err := engine.AddPattern(tier, pattern, flagPatternReason, "agent"); err != nil {
			return fmt.Errorf("invalid pattern: %w", err)
		}

		// Persist the pattern to SQLite. Without this, the in-memory
		// engine mutation above would die with the CLI process and
		// "patterns add" would be theatrical (issue #2). Open the
		// project DB and run migrations so a fresh `slb init` plus
		// `slb patterns add` works end-to-end without the user
		// having to think about schema.
		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return fmt.Errorf("opening project database to persist pattern: %w", err)
		}
		defer dbConn.Close()

		insertedID, err := dbConn.InsertCustomPattern(
			flagPatternTier,
			pattern,
			flagPatternReason,
			"agent",
		)
		if err != nil {
			if errors.Is(err, db.ErrCustomPatternExists) {
				// Idempotent re-add: don't fail. The in-memory
				// engine already accepted it (no-op after the first
				// call within a process), and the persistent row
				// is unchanged. Surface the existing id so JSON
				// consumers can distinguish "newly created" from
				// "already there".
				out := output.New(output.Format(GetOutput()))
				return out.Write(map[string]any{
					"status":   "already_exists",
					"id":       insertedID,
					"pattern":  pattern,
					"tier":     flagPatternTier,
					"reason":   flagPatternReason,
					"added_by": "agent",
				})
			}
			return fmt.Errorf("persisting pattern to database: %w", err)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"status":   "added",
			"id":       insertedID,
			"pattern":  pattern,
			"tier":     flagPatternTier,
			"reason":   flagPatternReason,
			"added_by": "agent",
		})
	},
}

var patternsRemoveCmd = &cobra.Command{
	Use:   "remove <pattern>",
	Short: "Remove a pattern (BLOCKED for agents)",
	Long: `Pattern removal is BLOCKED for agents.

Removing patterns makes the system less safe by potentially allowing
dangerous commands through without approval. This requires human oversight.

To remove a pattern, use 'slb tui' and navigate to pattern management,
or use 'slb patterns request-removal' to create a pending removal request.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := output.New(output.Format(GetOutput()))
		if GetOutput() == "json" {
			return out.Write(map[string]any{
				"error":   "pattern_removal_blocked",
				"message": "Pattern removal requires human approval. Use slb tui.",
			})
		}
		fmt.Fprintln(os.Stderr, "Error: Pattern removal requires human approval.")
		fmt.Fprintln(os.Stderr, "Use 'slb tui' to manage patterns, or 'slb patterns request-removal' to create a pending request.")
		os.Exit(1)
		return nil
	},
}

var patternsRequestRemovalCmd = &cobra.Command{
	Use:   "request-removal <pattern>",
	Short: "Request removal of a pattern (requires human review)",
	Long: `Create a pending removal request for a pattern.

This creates a request that must be approved by a human before
the pattern is actually removed. Use --reason to explain why
the pattern should be removed.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := args[0]

		if flagPatternReason == "" {
			return fmt.Errorf("--reason is required for removal requests")
		}

		// TODO: Implement pattern_changes table recording
		// For now, return a stub response
		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"status":     "pending",
			"request_id": "pending-impl",
			"pattern":    pattern,
			"reason":     flagPatternReason,
			"message":    "Removal request created. Awaiting human review in TUI.",
		})
	},
}

var patternsSuggestCmd = &cobra.Command{
	Use:   "suggest <pattern>",
	Short: "Suggest a pattern for human review",
	Long: `Suggest a new pattern for human review before it becomes active.

Unlike 'patterns add', suggested patterns are not immediately active.
A human must review and promote them through the TUI.

Use --tier to specify the suggested tier.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := args[0]

		if flagPatternTier == "" {
			return fmt.Errorf("--tier is required")
		}

		// TODO: Implement pattern_changes table with status='suggested'
		// For now, return a stub response
		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"status":  "suggested",
			"pattern": pattern,
			"tier":    flagPatternTier,
			"reason":  flagPatternReason,
			"message": "Pattern suggested. Awaiting human review in TUI.",
		})
	},
}

var patternsExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export patterns for external tools",
	Long: `Export all patterns in a format suitable for external tools.

Available formats:
  json        - Full JSON export with metadata (default)
  yaml        - YAML format
  claude-hook - Python code for Claude Code hooks

Examples:
  slb patterns export                         # JSON to stdout
  slb patterns export --format=claude-hook    # Python to stdout
  slb patterns export --output patterns.json        # JSON to file
  slb patterns export -f claude-hook --output hook.py  # Python to file`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		engine := core.GetDefaultEngine()

		var content string
		var err error

		switch strings.ToLower(flagPatternFormat) {
		case "json":
			content, err = engine.ExportJSON()
			if err != nil {
				return fmt.Errorf("failed to export JSON: %w", err)
			}
		case "claude-hook", "claude", "hook", "python":
			content = engine.ExportClaudeHook()
		case "yaml":
			// Export as JSON then convert to YAML-ish format
			export := engine.Export()
			data, err := json.MarshalIndent(export, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to export: %w", err)
			}
			content = string(data)
		default:
			return fmt.Errorf("unknown format: %s (use json, yaml, or claude-hook)", flagPatternFormat)
		}

		// Output to file or stdout
		if flagPatternOutputFile != "" {
			if err := os.WriteFile(flagPatternOutputFile, []byte(content), 0644); err != nil {
				return fmt.Errorf("failed to write file: %w", err)
			}
			// Confirm to user
			out := output.New(output.Format(GetOutput()))
			return out.Write(map[string]any{
				"status": "exported",
				"format": flagPatternFormat,
				"file":   flagPatternOutputFile,
				"hash":   engine.ComputeHash(),
				"count":  engine.Export().Metadata.PatternCount,
			})
		}

		// Print to stdout
		fmt.Print(content)
		return nil
	},
}

var patternsVersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show pattern version and hash",
	Long: `Show the current pattern version and SHA256 hash.

The hash changes when patterns are added/removed, enabling change detection.
Use this to verify if hook patterns need to be regenerated.

Examples:
  slb patterns version        # Show version info
  slb patterns version -j     # JSON output`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Reflect persisted customs in the reported sha256 +
		// pattern_count. Without this, `patterns version` would
		// always report the builtins-only hash even after a user
		// has added customs — and that hash is what tooling uses
		// to decide whether to regenerate the hook script.
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		engine := core.GetDefaultEngine()
		export := engine.Export()

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"version":       export.Version,
			"sha256":        export.SHA256,
			"pattern_count": export.Metadata.PatternCount,
			"tier_counts":   export.Metadata.TierCounts,
		})
	},
}

// Helper functions

func parseTier(s string) core.RiskTier {
	switch strings.ToLower(s) {
	case "critical":
		return core.RiskTierCritical
	case "dangerous":
		return core.RiskTierDangerous
	case "caution":
		return core.RiskTierCaution
	case "safe":
		return core.RiskTier(core.RiskSafe)
	default:
		return ""
	}
}

func outputPatterns(out *output.Writer, patterns map[string][]*core.Pattern) error {
	if GetOutput() == "json" {
		// JSON output: clean structure with snake_case
		result := make(map[string][]patternJSON)
		for tier, list := range patterns {
			plist := make([]patternJSON, 0, len(list))
			for _, p := range list {
				plist = append(plist, patternJSON{
					Pattern:     p.Pattern,
					Description: p.Description,
					Source:      p.Source,
				})
			}
			result[tier] = plist
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	// Text output: human-friendly
	tierOrder := []string{"safe", "critical", "dangerous", "caution"}
	for _, tier := range tierOrder {
		list, ok := patterns[tier]
		if !ok || len(list) == 0 {
			continue
		}
		fmt.Printf("\n%s (%d patterns):\n", strings.ToUpper(tier), len(list))
		for _, p := range list {
			fmt.Printf("  %s\n", p.Pattern)
			if p.Description != "" {
				fmt.Printf("    # %s\n", p.Description)
			}
		}
	}
	fmt.Println()
	return nil
}

type patternJSON struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source,omitempty"`
}
