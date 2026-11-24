package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// z_schema_wire.go initializes after all command registration init() functions
// (Go initializes files in lexical order within a package) and installs the
// global --schema flag interceptor across the command tree.
func init() {
	if rootCmd.PersistentFlags().Lookup("schema") == nil {
		rootCmd.PersistentFlags().BoolVar(&flagSchema, "schema", false, "emit JSON Schema for this command's structured output and exit")
	}
	installSchemaFlagSupport(rootCmd)
}

func schemaFlagEnabled(cmd *cobra.Command) bool {
	if flagSchema {
		return true
	}
	if cmd == nil {
		return false
	}
	if f := cmd.Flags().Lookup("schema"); f != nil && f.Changed {
		v, err := cmd.Flags().GetBool("schema")
		return err == nil && v
	}
	return false
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

	origArgs := cmd.Args
	cmd.Args = func(c *cobra.Command, args []string) error {
		if schemaFlagEnabled(c) {
			return nil
		}
		if origArgs != nil {
			return origArgs(c, args)
		}
		return nil
	}

	if cmd.PersistentPreRunE != nil {
		origPersistentPreRunE := cmd.PersistentPreRunE
		cmd.PersistentPreRunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return nil
			}
			return origPersistentPreRunE(c, args)
		}
	}

	if cmd.PreRunE != nil {
		origPreRunE := cmd.PreRunE
		cmd.PreRunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return nil
			}
			return origPreRunE(c, args)
		}
	}

	if cmd.RunE != nil {
		origRunE := cmd.RunE
		cmd.RunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return emitSchemaForCommand(c)
			}
			return origRunE(c, args)
		}
	} else if cmd.Run != nil {
		origRun := cmd.Run
		cmd.Run = nil
		cmd.RunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return emitSchemaForCommand(c)
			}
			origRun(c, args)
			return nil
		}
	} else if cmd != rootCmd {
		// Group command (e.g. `slb patterns --schema`, `slb daemon --schema`) with no Run/RunE.
		cmd.RunE = func(c *cobra.Command, args []string) error {
			if schemaFlagEnabled(c) {
				return emitSchemaForCommand(c)
			}
			return c.Help()
		}
	}
}
