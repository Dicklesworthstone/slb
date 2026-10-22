package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagDaemonStartForeground bool
	flagDaemonStopTimeoutSecs int
	flagDaemonLogsFollow      bool
	flagDaemonLogsLines       int
)

func init() {
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonLogsCmd)

	daemonStartCmd.Flags().BoolVar(&flagDaemonStartForeground, "foreground", false, "run the daemon in the current process (do not fork)")

	daemonStopCmd.Flags().IntVar(&flagDaemonStopTimeoutSecs, "timeout", 10, "seconds to wait for graceful shutdown")

	daemonLogsCmd.Flags().BoolVarP(&flagDaemonLogsFollow, "follow", "f", false, "follow the log output (tail -f)")
	daemonLogsCmd.Flags().IntVarP(&flagDaemonLogsLines, "lines", "n", 200, "number of lines to show")

	rootCmd.AddCommand(daemonCmd)
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the SLB daemon",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Override the root command's -C handling: resolve a relative flag or
		// SLB_PROJECT once, before changing directory, then keep the absolute
		// project root for every lifecycle command.
		project, err := daemonProjectPath()
		if err != nil {
			return err
		}
		if err := os.Chdir(project); err != nil {
			return fmt.Errorf("chdir to project: %w", err)
		}
		flagProject = project
		return nil
	},
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := daemonProjectPath()
		if err != nil {
			return err
		}
		startedAt := time.Now().UTC().Format(time.RFC3339)
		opts := daemon.ServerOptions{ProjectDir: project}
		socketPath := daemon.SocketPathForCWD(project)

		if flagDaemonStartForeground {
			out := output.New(output.Format(GetOutput()))
			_ = out.Write(map[string]any{
				"pid":         os.Getpid(),
				"socket_path": socketPath,
				"started_at":  startedAt,
				"foreground":  true,
			})
			return daemon.RunDaemon(context.Background(), opts)
		}

		if err := daemon.StartDaemonWithOptions(context.Background(), opts); err != nil {
			return err
		}

		info := daemon.NewClient(
			daemon.WithSocketPath(socketPath),
			daemon.WithPIDFile(daemon.PIDFileForCWD(project)),
			daemon.WithLocalOnly(),
		).GetStatusInfo()
		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"pid":         info.PID,
			"socket_path": info.SocketPath,
			"started_at":  startedAt,
			"foreground":  false,
		})
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := daemonProjectPath()
		if err != nil {
			return err
		}
		timeout := time.Duration(flagDaemonStopTimeoutSecs) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Second
		}

		if err := daemon.StopDaemonWithOptions(daemon.ServerOptions{ProjectDir: project}, timeout); err != nil {
			return err
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"stopped_at": time.Now().UTC().Format(time.RFC3339),
		})
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := daemonProjectPath()
		if err != nil {
			return err
		}
		client := daemon.NewClient(
			daemon.WithSocketPath(daemon.SocketPathForCWD(project)),
			daemon.WithPIDFile(daemon.PIDFileForCWD(project)),
			daemon.WithLocalOnly(),
		)
		info := client.GetStatusInfo()

		// Best-effort uptime based on pid file mtime.
		var uptimeSeconds int64
		var uptimeStr string
		if st, err := os.Stat(info.PIDFile); err == nil {
			uptime := time.Since(st.ModTime())
			if uptime > 0 {
				uptimeSeconds = int64(uptime.Seconds())
				uptimeStr = uptime.Truncate(time.Second).String()
			}
		}

		pendingCount, activeSessions := daemonProjectStats(project)

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"running":         info.Status == daemon.DaemonRunning,
			"status":          info.Status.String(),
			"pid":             info.PID,
			"pid_file":        info.PIDFile,
			"uptime":          uptimeStr,
			"uptime_seconds":  uptimeSeconds,
			"pending_count":   pendingCount,
			"active_sessions": activeSessions,
			"socket_path":     info.SocketPath,
			"socket_alive":    info.SocketAlive,
			"message":         info.Message,
		})
	},
}

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show daemon logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagDaemonLogsFollow && GetOutput() != "text" {
			return fmt.Errorf("--follow is only supported with text output")
		}

		path, err := daemonLogPath()
		if err != nil {
			return err
		}

		lines, err := tailFileLines(path, flagDaemonLogsLines)
		if err != nil {
			return err
		}

		if GetOutput() != "text" {
			out := output.New(output.Format(GetOutput()))
			return out.Write(map[string]any{
				"log_path": path,
				"lines":    lines,
			})
		}

		for _, line := range lines {
			fmt.Println(line)
		}

		if !flagDaemonLogsFollow {
			return nil
		}

		return followFile(path, os.Stdout)
	},
}

func daemonProjectPath() (string, error) {
	project := flagProject
	if project == "" {
		project = os.Getenv("SLB_PROJECT")
	}
	if project == "" {
		var err error
		project, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(project)
	if err != nil {
		return "", fmt.Errorf("resolving daemon project: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("resolving daemon project: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("daemon project is not a directory: %s", abs)
	}
	// Match the physical working directory used after -C, including aliases
	// that point into a project's descendants rather than its root.
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolving daemon project: %w", err)
	}
	return daemon.ProjectRootForCWD(abs), nil
}

func daemonProjectStats(projectPath string) (pendingCount int, activeSessions int) {
	dbPath := filepath.Join(projectPath, ".slb", "state.db")
	dbConn, err := db.OpenWithOptions(dbPath, db.OpenOptions{
		CreateIfNotExists: false,
		InitSchema:        false,
		ReadOnly:          true,
	})
	if err != nil {
		return 0, 0
	}
	defer dbConn.Close()

	if pending, err := dbConn.ListPendingRequests(projectPath); err == nil {
		pendingCount = len(pending)
	}
	if sessions, err := dbConn.ListActiveSessions(projectPath); err == nil {
		activeSessions = len(sessions)
	}
	return pendingCount, activeSessions
}

func daemonLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home dir: %w", err)
	}
	return filepath.Join(home, ".slb", "daemon.log"), nil
}

func tailFileLines(path string, n int) ([]string, error) {
	if n <= 0 {
		n = 200
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	r := bufio.NewScanner(f)
	// Allow long lines (structured logs).
	r.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	buf := make([]string, 0, n)
	for r.Scan() {
		line := r.Text()
		if len(buf) < n {
			buf = append(buf, line)
			continue
		}
		copy(buf, buf[1:])
		buf[len(buf)-1] = line
	}
	if err := r.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return buf, nil
}

func followFile(path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	// Seek to end; we already printed tail.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek %s: %w", path, err)
	}

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			_, _ = io.WriteString(w, line)
			continue
		}
		if errors.Is(err, io.EOF) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		return fmt.Errorf("tail %s: %w", path, err)
	}
}
