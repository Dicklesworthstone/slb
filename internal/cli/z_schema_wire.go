package cli

import (
	"fmt"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

var (
	schemaBypassMu     sync.Mutex
	schemaBypassedCmds = make(map[*cobra.Command]struct{})
	schemaFinalizeOnce sync.Once
)

func markBypassedFlagValidation(c *cobra.Command) {
	if c == nil {
		return
	}
	schemaBypassMu.Lock()
	defer schemaBypassMu.Unlock()
	if c.DisableFlagParsing {
		return
	}
	schemaBypassedCmds[c] = struct{}{}
	c.DisableFlagParsing = true
}

func restoreBypassedFlagValidation(c *cobra.Command) {
	if c == nil {
		return
	}
	schemaBypassMu.Lock()
	defer schemaBypassMu.Unlock()
	if _, bypassed := schemaBypassedCmds[c]; bypassed {
		delete(schemaBypassedCmds, c)
		c.DisableFlagParsing = false
	}
}

func restoreAllBypassedFlagValidation() {
	schemaBypassMu.Lock()
	defer schemaBypassMu.Unlock()
	for c := range schemaBypassedCmds {
		c.DisableFlagParsing = false
		delete(schemaBypassedCmds, c)
	}
}

// z_schema_wire.go initializes after all command registration init() functions
// (Go initializes files in lexical order within a package) and installs the
// global --schema flag interceptor across the command tree.
func init() {
	schemaFinalizeOnce.Do(func() {
		cobra.OnFinalize(restoreAllBypassedFlagValidation)
	})
	if rootCmd.PersistentFlags().Lookup("schema") == nil {
		rootCmd.PersistentFlags().BoolVar(&flagSchema, "schema", false, "emit JSON Schema for this command's structured output and exit")
	}
	installSchemaFlagSupport(rootCmd)
}

func schemaFlagEnabled(cmd *cobra.Command) bool {
	if cmd != nil {
		if f := cmd.Flags().Lookup("schema"); f != nil && f.Changed {
			v, err := cmd.Flags().GetBool("schema")
			return err == nil && v
		}
	}
	return flagSchema
}

func canonicalCommandKey(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	path := strings.TrimSpace(cmd.CommandPath())
	path = strings.TrimPrefix(path, "slb")
	return normalizeSchemaKey(path)
}

func emitSchemaForCommand(cmd *cobra.Command) error {
	key := canonicalCommandKey(cmd)
	if key == "" {
		return writeSchemaCatalog(cmd.OutOrStdout())
	}
	schema, err := LookupCommandSchema(key)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	return writeSchemaValue(cmd.OutOrStdout(), schema)
}

func installSchemaFlagSupport(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	schemaFinalizeOnce.Do(func() {
		cobra.OnFinalize(restoreAllBypassedFlagValidation)
	})
	wrapCommandForSchema(cmd)
	for _, sub := range cmd.Commands() {
		installSchemaFlagSupport(sub)
	}
}

func wrapCommandForSchema(cmd *cobra.Command) {
	if cmd.Annotations != nil && cmd.Annotations["slb.schema.wrapped"] == "1" {
		return
	}
	if cmd.Annotations == nil {
		cmd.Annotations = make(map[string]string)
	}
	cmd.Annotations["slb.schema.wrapped"] = "1"

	isGroupCmd := cmd != rootCmd && cmd.RunE == nil && cmd.Run == nil && len(cmd.Commands()) > 0
	origArgs := cmd.Args
	cmd.Args = func(c *cobra.Command, args []string) error {
		if isGroupCmd && origArgs == nil && len(args) > 0 {
			return fmt.Errorf("unknown command %q for %q", args[0], c.CommandPath())
		}
		if schemaFlagEnabled(c) {
			return nil
		}
		if origArgs != nil {
			return origArgs(c, args)
		}
		return nil
	}

	// Only wrap persistent hooks when already present so we never shadow
	// inherited parent hooks during normal (non-schema) execution.
	if cmd.PersistentPreRunE != nil {
		origPersistentPreRunE := cmd.PersistentPreRunE
		cmd.PersistentPreRunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return nil
			}
			restoreBypassedFlagValidation(c)
			return origPersistentPreRunE(c, args)
		}
	} else if cmd.PersistentPreRun != nil {
		origPersistentPreRun := cmd.PersistentPreRun
		cmd.PersistentPreRun = nil
		cmd.PersistentPreRunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return nil
			}
			restoreBypassedFlagValidation(c)
			origPersistentPreRun(c, args)
			return nil
		}
	}

	// Always install PreRunE so --schema bypasses Cobra's ValidateRequiredFlags
	// and ValidateFlagGroups (which run between PreRunE and RunE and return nil
	// when DisableFlagParsing is set). RunE, non-schema PreRunE, and Cobra's
	// OnFinalize hook ensure DisableFlagParsing is always restored even on panic
	// or early exit.
	origPreRunE := cmd.PreRunE
	origPreRun := cmd.PreRun
	cmd.PreRun = nil
	cmd.PreRunE = func(c *cobra.Command, args []string) error {
		if schemaFlagEnabled(c) {
			markBypassedFlagValidation(c)
			return nil
		}
		restoreBypassedFlagValidation(c)
		if origPreRunE != nil {
			return origPreRunE(c, args)
		}
		if origPreRun != nil {
			origPreRun(c, args)
		}
		return nil
	}

	if cmd == schemaCmd && cmd.RunE != nil {
		// schemaCmd handles --schema, --all, and positional command arguments
		// directly so `slb schema pending --schema` and `slb schema --all --schema`
		// honor their arguments/flags while `slb schema --schema` emits the
		// schema catalog's own JSON Schema.
		origRunE := cmd.RunE
		cmd.RunE = func(c *cobra.Command, args []string) error {
			defer restoreBypassedFlagValidation(c)
			restoreBypassedFlagValidation(c)
			return origRunE(c, args)
		}
	} else if cmd.RunE != nil {
		origRunE := cmd.RunE
		cmd.RunE = func(c *cobra.Command, args []string) error {
			defer restoreBypassedFlagValidation(c)
			restoreBypassedFlagValidation(c)
			if schemaFlagEnabled(c) {
				return emitSchemaForCommand(c)
			}
			return origRunE(c, args)
		}
	} else if cmd.Run != nil {
		origRun := cmd.Run
		cmd.Run = nil
		cmd.RunE = func(c *cobra.Command, args []string) error {
			defer restoreBypassedFlagValidation(c)
			restoreBypassedFlagValidation(c)
			if schemaFlagEnabled(c) {
				return emitSchemaForCommand(c)
			}
			origRun(c, args)
			return nil
		}
	} else if cmd != rootCmd {
		// Group command (e.g. `slb patterns --schema`, `slb daemon --schema`) with no Run/RunE.
		cmd.RunE = func(c *cobra.Command, args []string) error {
			defer restoreBypassedFlagValidation(c)
			restoreBypassedFlagValidation(c)
			if schemaFlagEnabled(c) {
				return emitSchemaForCommand(c)
			}
			return c.Help()
		}
	}

	if cmd.PostRunE != nil {
		origPostRunE := cmd.PostRunE
		cmd.PostRunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return nil
			}
			return origPostRunE(c, args)
		}
	} else if cmd.PostRun != nil {
		origPostRun := cmd.PostRun
		cmd.PostRun = nil
		cmd.PostRunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return nil
			}
			origPostRun(c, args)
			return nil
		}
	}

	if cmd.PersistentPostRunE != nil {
		origPersistentPostRunE := cmd.PersistentPostRunE
		cmd.PersistentPostRunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return nil
			}
			return origPersistentPostRunE(c, args)
		}
	} else if cmd.PersistentPostRun != nil {
		origPersistentPostRun := cmd.PersistentPostRun
		cmd.PersistentPostRun = nil
		cmd.PersistentPostRunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return nil
			}
			origPersistentPostRun(c, args)
			return nil
		}
	}
}
