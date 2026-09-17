package cli

import (
	"fmt"
	"os"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// loadCustomPatternsIntoDefaultEngine replaces the active snapshot with the
// selected project's persisted policy. Only an absent default database permits
// a builtins-only classification before initialization. An explicit --db must
// exist, and corrupt/unreadable databases and invalid rules are hard errors.
// Read-only classification must not create or migrate a database as a side
// effect. Callers must propagate errors rather than classify with stale rules.
func loadCustomPatternsIntoDefaultEngine() (int, error) {
	path := GetDB()
	info, err := os.Stat(path)
	if os.IsNotExist(err) && flagDB == "" {
		return core.GetDefaultEngine().ReplaceCustomPatterns(nil)
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
		tier := parseTier(row.Tier)
		if tier == "" {
			return 0, fmt.Errorf("invalid persisted pattern tier %q", row.Tier)
		}
		patterns = append(patterns, core.Pattern{
			Tier: tier, Pattern: row.Pattern, Description: row.Description, Source: row.Source,
		})
	}
	return core.GetDefaultEngine().ReplaceCustomPatterns(patterns)
}
