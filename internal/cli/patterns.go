package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
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

func init() {
	// patterns command
	patternsCmd.PersistentFlags().StringVarP(&flagPatternTier, "tier", "T", "", "risk tier (critical, dangerous, caution, safe)")
	patternsCmd.PersistentFlags().StringVarP(&flagPatternReason, "reason", "r", "", "reason for adding/removing pattern")

	// patterns test/check flags
	patternsTestCmd.Flags().BoolVar(&flagPatternExitCode, "exit-code", false, "return non-zero exit code if approval needed")

	// --output/-o is the persistent output format, not a destination path.
	patternsExportCmd.Flags().StringVarP(&flagPatternFormat, "format", "f", "json", "export format: json, yaml, claude-hook")
	patternsExportCmd.Flags().StringVar(&flagPatternOutputFile, "output-file", "", "output file (default: stdout)")

	patternsCmd.AddCommand(patternsListCmd)
	patternsCmd.AddCommand(patternsTestCmd)
	patternsCmd.AddCommand(patternsAddCmd)
	patternsCmd.AddCommand(patternsRemoveCmd)
	patternsCmd.AddCommand(patternsRequestRemovalCmd)
	patternsCmd.AddCommand(patternsSuggestCmd)
	patternsCmd.AddCommand(patternsExportCmd)
	patternsCmd.AddCommand(patternsVersionCmd)

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

Agents can add restrictive patterns, but SAFE additions and custom-pattern
removals require human approval through the TUI.`,
}

var patternsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all patterns grouped by tier",
	Long: `List all patterns used for command classification.

Use --tier to filter by a specific tier (safe, critical, dangerous, caution).
Without --tier, all patterns from all tiers are shown.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			return err
		}
		engine := core.GetDefaultEngine()
		out := output.New(output.Format(GetOutput()))

		if flagPatternTier != "" {
			tier := parseTier(flagPatternTier)
			if tier == "" && flagPatternTier != "safe" {
				return fmt.Errorf("invalid tier: %s (must be safe, critical, dangerous, or caution)", flagPatternTier)
			}
			patterns := engine.ListPatterns(tier)
			return outputPatterns(out, map[string][]*core.Pattern{flagPatternTier: patterns})
		}

		return outputPatterns(out, engine.AllPatterns())
	},
}

var patternsTestCmd = &cobra.Command{
	Use:   "test <command>",
	Short: "Test which tier a command matches",
	Long: `Test a command against all patterns and show its risk classification.

Returns the tier, matched pattern, minimum approvals required, and whether
approval is needed.

Use --exit-code to return non-zero exit code if approval is needed.
This is useful for Claude Code hooks integration.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			return err
		}

		command := args[0]
		cwd, _ := os.Getwd()
		result := core.Classify(command, cwd)
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
					"segment": seg.Segment, "tier": string(seg.Tier), "matched_pattern": seg.MatchedPattern,
				})
			}
			resp["matched_segments"] = segments
		}

		format := GetOutput()
		if format == "text" {
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

		if flagPatternExitCode && result.NeedsApproval {
			os.Stdout.Sync()
			os.Exit(1)
		}
		return nil
	},
}

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

Restrictive patterns take effect immediately. SAFE patterns are queued for
human review because allowlist entries can bypass dangerous-command checks.
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
		if tier == "" {
			return fmt.Errorf("invalid tier: %s", flagPatternTier)
		}
		// Validate without changing the active engine. A failed database write
		// must not leave a new in-memory allow rule behind.
		if _, err := regexp.Compile("(?i)" + pattern); err != nil {
			return fmt.Errorf("invalid pattern: %w", err)
		}
		if tier == core.RiskTier(core.RiskSafe) {
			return queuePatternChange(pattern, string(tier), db.PatternChangeTypeAdd, flagPatternReason)
		}

		conn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return fmt.Errorf("opening project database to persist pattern: %w", err)
		}
		defer conn.Close()
		id, err := conn.InsertCustomPattern(string(tier), pattern, flagPatternReason, "agent")
		status := "added"
		if errors.Is(err, db.ErrCustomPatternExists) {
			status = "already_exists"
		} else if err != nil {
			return fmt.Errorf("persisting pattern to database: %w", err)
		}
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			return err
		}
		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"status": status, "id": id, "pattern": pattern,
			"tier": string(tier), "reason": flagPatternReason, "added_by": "agent",
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

This records a request that must be approved by a human before the custom
pattern is removed. Use --reason to explain why and --tier to disambiguate.
Builtin rules are not removed by this workflow.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := args[0]
		if strings.TrimSpace(flagPatternReason) == "" {
			return fmt.Errorf("--reason is required for removal requests")
		}
		return queuePatternChange(pattern, flagPatternTier, db.PatternChangeTypeRemove, flagPatternReason)
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
		return queuePatternChange(pattern, flagPatternTier, db.PatternChangeTypeSuggest, flagPatternReason)
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
  slb patterns export                              # JSON to stdout
  slb patterns export --format=claude-hook         # Python to stdout
  slb patterns export --output-file patterns.json  # JSON to file
  slb patterns export -f claude-hook --output-file hook.py  # Python to file`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			return err
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
			export := engine.Export()
			data, err := json.MarshalIndent(export, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to export: %w", err)
			}
			content = string(data)
		default:
			return fmt.Errorf("unknown format: %s (use json, yaml, or claude-hook)", flagPatternFormat)
		}
		if flagPatternOutputFile != "" {
			if err := os.WriteFile(flagPatternOutputFile, []byte(content), 0644); err != nil {
				return fmt.Errorf("failed to write file: %w", err)
			}
			out := output.New(output.Format(GetOutput()))
			return out.Write(map[string]any{
				"status": "exported", "format": flagPatternFormat, "file": flagPatternOutputFile,
				"hash": engine.ComputeHash(), "count": engine.Export().Metadata.PatternCount,
			})
		}
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
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			return err
		}
		export := core.GetDefaultEngine().Export()
		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"version": export.Version, "sha256": export.SHA256,
			"pattern_count": export.Metadata.PatternCount, "tier_counts": export.Metadata.TierCounts,
		})
	},
}

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
		result := make(map[string][]patternJSON)
		for tier, list := range patterns {
			plist := make([]patternJSON, 0, len(list))
			for _, p := range list {
				plist = append(plist, patternJSON{Pattern: p.Pattern, Description: p.Description, Source: p.Source})
			}
			result[tier] = plist
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
		return nil
	}
	for _, tier := range []string{"safe", "critical", "dangerous", "caution"} {
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
