package cli

import (
	"fmt"

	"github.com/Dicklesworthstone/slb/internal/tui"
	"github.com/spf13/cobra"
)

var (
	flagTuiNoMouse        bool
	flagTuiRefreshSeconds int
	flagTuiTheme          string
	flagTuiSessionID      string
	flagTuiSessionKey     string
)

func init() {
	tuiCmd.Flags().BoolVar(&flagTuiNoMouse, "no-mouse", false, "disable mouse support")
	tuiCmd.Flags().IntVar(&flagTuiRefreshSeconds, "refresh-interval", 5, "request detail refresh interval (seconds)")
	tuiCmd.Flags().StringVar(&flagTuiTheme, "theme", "", "override theme (mocha, macchiato, frappe, latte)")
	tuiCmd.Flags().StringVar(&flagTuiSessionID, "session-id", "", "session ID for approvals")
	tuiCmd.Flags().StringVar(&flagTuiSessionKey, "session-key", "", "session key for approvals")

	rootCmd.AddCommand(tuiCmd)
}

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the interactive TUI dashboard",
	Long: `Launch the SLB Bubble Tea dashboard.

The dashboard and open request details refresh from the selected database.
--db applies to every view, refresh, review and pattern action. The selected
database must already exist; opening failures are reported before the UI starts.
Project identity still scopes requests and determines the review policy.
Providing --session-id and --session-key enables interactive approval/rejection.
Reviews obey the request project's policy and --config override. A recorded
vote is not full approval until the configured quorum is met. No command is
executed by submitting a review.

Key bindings:
  tab/shift+tab  Switch between panels
  up/down (j/k)  Navigate within panels
  enter          View selected request details
  a/r            Open approval/rejection form in request details
  ctrl+s         Submit the review form
  esc            Cancel the form, or leave request details
  f5/ctrl+r      Refresh request details immediately (outside a form)
  m              Pattern management
  H              History browser
  q              Quit

Theme options: mocha (default), macchiato, frappe, latte`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := resolveTUIOptions()
		if err != nil {
			return err
		}

		if err := tui.RunWithOptions(opts); err != nil {
			return fmt.Errorf("tui: %w", err)
		}
		return nil
	},
}

func resolveTUIOptions() (tui.Options, error) {
	project, err := projectPath()
	if err != nil {
		return tui.Options{}, fmt.Errorf("resolving project: %w", err)
	}
	database, err := GetDB()
	if err != nil {
		return tui.Options{}, err
	}
	if flagTuiRefreshSeconds <= 0 {
		return tui.Options{}, fmt.Errorf("--refresh-interval must be positive")
	}
	return tui.ResolveOptions(tui.Options{
		ProjectPath: project, DatabasePath: database, ConfigPath: flagConfig,
		Theme: flagTuiTheme, DisableMouse: flagTuiNoMouse,
		RefreshInterval: flagTuiRefreshSeconds,
		SessionID:       flagTuiSessionID, SessionKey: flagTuiSessionKey,
	})
}
