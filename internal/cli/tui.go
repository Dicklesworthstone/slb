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
)

func init() {
	tuiCmd.Flags().BoolVar(&flagTuiNoMouse, "no-mouse", false, "disable mouse support")
	tuiCmd.Flags().IntVar(&flagTuiRefreshSeconds, "refresh-interval", 5, "polling interval when no daemon (seconds)")
	tuiCmd.Flags().StringVar(&flagTuiTheme, "theme", "", "override theme (mocha, macchiato, latte, nord)")

	rootCmd.AddCommand(tuiCmd)
}

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the interactive TUI dashboard",
	Long: `Launch the SLB Bubble Tea dashboard.

If the daemon is running, live updates are streamed; otherwise polling is used.
Press q to quit.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := tui.Run(); err != nil {
			return fmt.Errorf("tui: %w", err)
		}
		return nil
	},
}
