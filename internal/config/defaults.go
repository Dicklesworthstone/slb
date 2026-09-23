package config

// Built-in defaults are also the PatternEngine's source of truth. Never keep
// a second, weaker allowlist in configuration than the one used at runtime.
var (
	defaultCriticalPatterns = []string{
		`^rm\s+(-[rf]+\s+)+["']?/+(?:\.\.?/+)*(boot|dev|etc|home|lib|lib64|media|mnt|opt|proc|root|run|sbin|srv|sys|usr|var)(?:[/\s"'*]|$)`,
		`^rm\s+(-[rf]+\s+)+/($|\s)`,
		`^rm\s+(-[rf]+\s+)+/\*`,
		`^rm\s+(-[rf]+\s+)+~`,
		`DROP\s+DATABASE`,
		`DROP\s+SCHEMA`,
		`TRUNCATE\s+TABLE`,
		`DELETE\s+FROM\s+[\w.` + "`" + `"\[\]]+\s*(;|$|--|/\*)`,
		`^terraform\s+destroy\s*$`,
		`^terraform\s+destroy\s+-auto-approve`,
		`^terraform\s+destroy\s+[^-]`,
		`^kubectl\s+delete\s+(node|nodes|namespace|namespaces|pv|persistentvolume|pvc|persistentvolumeclaim)\b`,
		`^helm\s+uninstall.*--all`,
		`^docker\s+system\s+prune\s+-a`,
		`^git\s+push\s+.*--force($|\s)`,
		`^git\s+push\s+(?:.*\s)?-[a-z]*f[a-z]*($|\s)`,
		`^aws\s+.*terminate-instances`,
		`^gcloud\s+(?:.*\s)?delete(?:\s.*)?\s(?:--quiet|-q)($|\s)`,
		`\bdd\b.*of=/dev/`,
		`^mkfs`,
		`^fdisk`,
		`^parted`,
		`^chmod\s+(?:.*[\s"'])?/+(?:\.\.?/+)*(etc|usr|var|boot|bin|sbin)(?:[/\s"'*]|$)`,
		`^chown\s+(?:.*[\s"'])?/+(?:\.\.?/+)*(etc|usr|var|boot|bin|sbin)(?:[/\s"'*]|$)`,
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
		`^rm$`,
		`^git\s+stash\s+drop`,
		`^git\s+branch\s+-[dD]`,
		`^npm\s+uninstall`,
		`^pip\s+uninstall`,
		`^cargo\s+remove`,
	}
	defaultSafePatterns = []string{
		// Every target must be a throwaway file; a trailing .log cannot
		// turn a recursive deletion of other paths into a safe command.
		`^rm\s+(?:-[fv]+\s+|--(?:force|verbose)\s+)*(?:--\s+)?(?:\S+\.log\s+)*\S+\.log$`,
		`^rm\s+(?:-[fv]+\s+|--(?:force|verbose)\s+)*(?:--\s+)?(?:\S+\.tmp\s+)*\S+\.tmp$`,
		`^rm\s+(?:-[fv]+\s+|--(?:force|verbose)\s+)*(?:--\s+)?(?:\S+\.bak\s+)*\S+\.bak$`,
		`^git\s+stash\s*$`,
		`^kubectl\s+delete\s+pod\s`,
		`^npm\s+cache\s+clean`,
	}
)

// DefaultConfig returns independently owned defaults. A caller changing a
// configured array must never mutate defaults used by another project.
func DefaultConfig() Config {
	return Config{
		General: GeneralConfig{
			MinApprovals: 2, RequireDifferentModel: false, DifferentModelTimeoutSecs: 300,
			ConflictResolution: "any_rejection_blocks", RequestTimeoutSecs: 1800,
			ApprovalTTLMins: 30, ApprovalTTLCriticalMins: 10, TimeoutAction: "escalate",
			EnableDryRun: true, EnableRollbackCapture: true, MaxRollbackSizeMB: 100,
			CrossProjectReviews: false, ReviewPool: []string{},
		},
		Daemon: DaemonConfig{
			UseFileWatcher: true, IPCSocket: "", TCPAddr: "", TCPRequireAuth: true,
			TCPAllowedIPs: []string{}, LogLevel: "info", PIDFile: "",
		},
		RateLimits:    RateLimitConfig{MaxPendingPerSession: 5, MaxRequestsPerMinute: 10, RateLimitAction: "reject"},
		Notifications: NotificationsConfig{DesktopEnabled: true, DesktopDelaySecs: 60, WebhookURL: "", EmailEnabled: false},
		History:       HistoryConfig{DatabasePath: "", GitRepoPath: "", RetentionDays: 365, AutoGitCommit: true},
		Patterns: PatternsConfig{
			Critical: PatternTierConfig{
				MinApprovals: 2, DynamicQuorum: false, DynamicQuorumFloor: 2,
				AutoApproveDelaySeconds: 0, Patterns: append([]string(nil), defaultCriticalPatterns...),
			},
			Dangerous: PatternTierConfig{
				MinApprovals: 1, DynamicQuorum: false, DynamicQuorumFloor: 1,
				AutoApproveDelaySeconds: 0, Patterns: append([]string(nil), defaultDangerousPatterns...),
			},
			Caution: PatternTierConfig{
				MinApprovals: 0, DynamicQuorum: false, DynamicQuorumFloor: 0,
				AutoApproveDelaySeconds: 30, Patterns: append([]string(nil), defaultCautionPatterns...),
			},
			Safe: PatternTierConfig{
				MinApprovals: 0, DynamicQuorum: false, DynamicQuorumFloor: 0,
				AutoApproveDelaySeconds: 0, Patterns: append([]string(nil), defaultSafePatterns...),
			},
		},
		Integrations: IntegrationsConfig{AgentMailEnabled: true, AgentMailThread: "SLB-Reviews", ClaudeHooksEnabled: true, HookCautionAction: "block", HookQueryTimeoutMS: DefaultHookQueryTimeoutMS},
		Agents:       AgentsConfig{TrustedSelfApprove: []string{}, TrustedSelfApproveDelaySecs: 300, Blocked: []string{}},
	}
}
