package cli

import (
	"fmt"
	"os"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// loadCustomPatternsIntoDefaultEngine publishes the complete effective policy:
// defaults < user config < project (or explicit) config < environment,
// followed by the project's persisted custom rules. Read-only classification
// never creates a database, and a load failure never publishes partial policy.
func loadCustomPatternsIntoDefaultEngine() (int, error) {
	project, err := projectPath()
	if err != nil {
		return 0, err
	}
	cfg, err := config.Load(config.LoadOptions{ProjectDir: project, ConfigPath: flagConfig})
	if err != nil {
		return 0, fmt.Errorf("loading command policy config: %w", err)
	}
	if flagConfig != "" {
		if info, err := os.Stat(flagConfig); err != nil {
			return 0, fmt.Errorf("reading explicit policy config: %w", err)
		} else if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("explicit policy config is not a regular file")
		}
	}
	path := GetDB()
	info, err := os.Stat(path)
	if os.IsNotExist(err) && flagDB == "" {
		return core.GetDefaultEngine().ReplacePolicy(cfg, nil, flagConfig)
	}
	if err != nil {
		return 0, fmt.Errorf("reading project policy database %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("project policy database %q is not a regular file", path)
	}
	conn, err := db.OpenWithOptions(path, db.OpenOptions{ReadOnly: true})
	if err != nil {
		return 0, fmt.Errorf("opening project policy database %q: %w", path, err)
	}
	defer conn.Close()
	rows, err := conn.ListCustomPatterns()
	if err != nil {
		return 0, fmt.Errorf("loading project policy: %w", err)
	}
	patterns := make([]core.Pattern, 0, len(rows))
	for _, row := range rows {
		patterns = append(patterns, core.Pattern{Tier: core.RiskTier(row.Tier),
			Pattern: row.Pattern, Description: row.Description, Source: row.Source})
	}
	return core.GetDefaultEngine().ReplacePolicy(cfg, patterns, flagConfig)
}
