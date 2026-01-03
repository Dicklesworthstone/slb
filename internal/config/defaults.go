package config

// Built-in defaults for SLB configuration.
// These should stay aligned with the values in PLAN_TO_MAKE_SLB.md.

var (
	defaultCriticalPatterns = []string{
		`^rm\s+(-[rf]+\s+)+/(etc|usr|var|boot|home|root|bin|sbin|lib)`,
		`^rm\s+(-[rf]+\s+)+/[^t]`,
		`^rm\s+(-[rf]+\s+)+~`,
		`DROP\s+DATABASE`,
		`DROP\s+SCHEMA`,
		`TRUNCATE\s+TABLE`,
		`DELETE\s+FROM\s+[\w.` + "`" + `"\[\]]+\s*(;|$|--|/\*)`,
		`^terraform\s+destroy\s*$`,
		`^terraform\s+destroy\s+[^-]`,
		`^kubectl\s+delete\s+(node|namespace|pv|pvc)\b`,
		`^helm\s+uninstall.*--all`,
		`^docker\s+system\s+prune\s+-a`,
		`^git\s+push\s+.*--force($|\s)`,
		`^aws\s+.*terminate-instances`,
		`^gcloud.*delete.*--quiet`,
	}

	defaultDangerousPatterns = []string{
		`^rm\s+-[rf]{2}`, // -rf or -fr (order-independent)
		`^rm\s+-r`,
		`^git\s+reset\s+--hard`,
		`^git\s+clean\s+-fd`,
		`^git\s+push.*--force-with-lease`,
		`^kubectl\s+delete`,
		`^helm\s+uninstall`,
		`^docker\s+rm`,
		`^docker\s+rmi`,
		`^terraform\s+destroy.*-target`,
		`^terraform\s+state\s+rm`,
		`DROP\s+TABLE`,
		`DELETE\s+FROM.*WHERE`,
		`^chmod\s+-R`,
		`^chown\s+-R`,
	}

	defaultCautionPatterns = []string{
		`^rm\s+[^-]`,
		`^git\s+stash\s+drop`,
		`^git\s+branch\s+-[dD]`,
		`^npm\s+uninstall`,
		`^pip\s+uninstall`,
		`^cargo\s+remove`,
	}

	defaultSafePatterns = []string{
		`^rm\s+.*\.log$`,
		`^rm\s+.*\.tmp$`,
		`^rm\s+.*\.bak$`,
		`^git\s+stash\s*$`,
		`^kubectl\s+delete\s+pod\s`,
		`^npm\s+cache\s+clean`,
	}
)

// DefaultConfig returns the built-in default configuration.
func DefaultConfig() Config {
	return Config{
		General: GeneralConfig{
			MinApprovals:              2,
			RequireDifferentModel:     false,
			DifferentModelTimeoutSecs: 300,
			ConflictResolution:        "any_rejection_blocks",
			RequestTimeoutSecs:        1800,
			ApprovalTTLMins:           30,
			ApprovalTTLCriticalMins:   10,
			TimeoutAction:             "escalate",
			EnableDryRun:              true,
			EnableRollbackCapture:     true,
			MaxRollbackSizeMB:         100,
			CrossProjectReviews:       false,
			ReviewPool:                []string{},
		},
		Daemon: DaemonConfig{
			UseFileWatcher: true,
			IPCSocket:      "",
			TCPAddr:        "",
			TCPRequireAuth: true,
			TCPAllowedIPs:  []string{},
			LogLevel:       "info",
			PIDFile:        "",
		},
		RateLimits: RateLimitConfig{
			MaxPendingPerSession: 5,
			MaxRequestsPerMinute: 10,
			RateLimitAction:      "reject",
		},
		Notifications: NotificationsConfig{
			DesktopEnabled:   true,
			DesktopDelaySecs: 60,
			WebhookURL:       "",
			EmailEnabled:     false,
		},
		History: HistoryConfig{
			DatabasePath:  "",
			GitRepoPath:   "",
			RetentionDays: 365,
			AutoGitCommit: true,
		},
		Patterns: PatternsConfig{
			Critical: PatternTierConfig{
				MinApprovals:            2,
				DynamicQuorum:           false,
				DynamicQuorumFloor:      2,
				AutoApproveDelaySeconds: 0,
				Patterns:                defaultCriticalPatterns,
			},
			Dangerous: PatternTierConfig{
				MinApprovals:            1,
				DynamicQuorum:           false,
				DynamicQuorumFloor:      1,
				AutoApproveDelaySeconds: 0,
				Patterns:                defaultDangerousPatterns,
			},
			Caution: PatternTierConfig{
				MinApprovals:            0,
				DynamicQuorum:           false,
				DynamicQuorumFloor:      0,
				AutoApproveDelaySeconds: 30,
				Patterns:                defaultCautionPatterns,
			},
			Safe: PatternTierConfig{
				MinApprovals:            0,
				DynamicQuorum:           false,
				DynamicQuorumFloor:      0,
				AutoApproveDelaySeconds: 0,
				Patterns:                defaultSafePatterns,
			},
		},
		Integrations: IntegrationsConfig{
			AgentMailEnabled:   true,
			AgentMailThread:    "SLB-Reviews",
			ClaudeHooksEnabled: true,
		},
		Agents: AgentsConfig{
			TrustedSelfApprove:          []string{},
			TrustedSelfApproveDelaySecs: 300,
			Blocked:                     []string{},
		},
	}
}
