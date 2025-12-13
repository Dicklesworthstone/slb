# PLAN_TO_MAKE_SLB.md

## Simultaneous Launch Button (slb) — Multi-Agent Command Authorization System

---

## Executive Summary

**slb** is a cross-platform CLI tool implementing a "two-person rule" (inspired by nuclear launch protocols) for potentially destructive commands executed by AI coding agents. When an agent wants to run a dangerous command (e.g., `rm -rf`, `kubectl delete node`, `DROP DATABASE`), it must submit the command for peer review by another agent. Only when a second agent independently evaluates the reasoning and approves does the command execute.

**Core insight**: AI agents can hallucinate, get tunnel vision, or misunderstand context. A fresh perspective from a second agent—especially one with different training/architecture—catches errors before they become irreversible disasters.

**Primary use case**: Multiple Claude Code / Codex / other AI agents working in parallel on the same codebase, where one agent's mistake could destroy another's work or critical infrastructure.

---

## Problem Statement

### The Scenario

You're running 30 terminal windows with AI agents working in parallel. Agent #17 decides it needs to clean up some files and runs `rm -rf /important-data`. Or it "helpfully" deletes your Kubernetes nodes because it misunderstood the task.

### Why This Happens

1. **Tunnel vision**: Agents focus on their immediate task and lose sight of broader context
2. **Hallucination**: Agents confidently execute commands based on incorrect assumptions
3. **Missing context**: Agent doesn't know what other agents are doing
4. **Overconfidence**: Agent believes a destructive action is "obviously correct"
5. **Ambiguous instructions**: Human's intent was misinterpreted

### The Solution

Before executing any command matching "dangerous" patterns, agents must:
1. Submit the command with detailed justification
2. Wait for independent review by another agent
3. Only proceed after explicit approval

This creates a **deliberate friction point** that forces reconsideration of destructive actions.

---

## Critical Design Feedback

### Issues with Original Proposal

#### 1. Single Lock File is Problematic

**Original idea**: Create `.slb_command_lock_file` in the project folder.

**Problems**:
- Multiple pending commands would overwrite each other
- Race conditions between agents
- Git noise if not properly ignored
- No support for queued commands

**Better approach**:
```
.slb/
  pending/
    req-a1b2c3d4.json    # Each request gets unique file
    req-e5f6g7h8.json
  processed/
    2025-12-13/
      req-x9y0z1.json    # Archived after resolution
  sessions/
    agent-GreenLake.json # Active agent registrations
  config.toml            # Project-level configuration
```

#### 2. Polling Loop is Wasteful

**Original idea**: "slb is running in a loop the entire time in the background (using very low resources)"

**Problem**: Polling loops waste CPU cycles and battery, even when "low resource."

**Better approach**:
- Use native file system watchers (inotify on Linux, FSEvents on macOS, ReadDirectoryChangesW on Windows)
- Go's fsnotify provides cross-platform file watching
- Only wake up when files actually change
- Alternatively: Unix domain socket for immediate IPC between agents and daemon

#### 3. "Different Model" Requirement is Hard to Enforce

**Original idea**: Optionally require approver to be different model (e.g., Claude can't approve Claude).

**Problem**: Self-reported model info has no cryptographic verification.

**Solution**:
- Accept this as advisory/honor-system rather than security guarantee
- Log model info for audit purposes
- The real value is the second perspective, not the model difference
- Optional strict mode where human must approve if same-model

#### 4. What Counts as "Dangerous"?

**Missing**: Clear definition of which commands need review.

**Solution**: Tiered pattern matching system (detailed below)

---

## My Creative Additions

### 1. Pre-flight Dry Run

For commands that support it, automatically run a dry-run version:

```bash
# Original command:
kubectl delete deployment my-app

# slb automatically runs:
kubectl delete deployment my-app --dry-run=client -o yaml
```

The dry-run output is attached to the request, giving reviewers concrete information about what WILL happen, not just what the requestor CLAIMS will happen.

**Supported commands**:
- `kubectl *` → `--dry-run=client`
- `terraform destroy` → `terraform plan -destroy`
- `rm` → show file listing of what would be deleted
- `git reset --hard` → `git diff HEAD..<target>`

### 2. Three-Tier Risk Classification

Not all dangerous commands are equally dangerous:

| Tier | Risk Level | Default Behavior | Examples |
|------|-----------|------------------|----------|
| **CRITICAL** | Catastrophic, irreversible | 2+ approvals required, human notification | `rm -rf /`, `DROP DATABASE`, `terraform destroy`, `kubectl delete node` |
| **DANGEROUS** | Significant, recoverable with effort | 1 approval required | `git reset --hard`, `rm -rf <dir>`, `kubectl delete deployment` |
| **CAUTION** | Annoying if wrong, easily fixed | Auto-approved after 30s with logging | `rm *.log`, `git clean`, `npm uninstall` |

Users configure patterns into tiers. Sensible defaults provided.

### 3. Rollback Capture

Before executing CRITICAL/DANGEROUS commands, optionally capture state:

```bash
# Before: kubectl delete deployment my-app
# slb captures: kubectl get deployment my-app -o yaml > .slb/rollback/req-xxx/my-app.yaml

# Before: rm -rf ./data/
# slb captures: tar -czf .slb/rollback/req-xxx/data-backup.tar.gz ./data/
```

Enables one-command rollback from TUI if things go wrong.

### 4. Context Attachments

Requesting agent can attach supporting evidence:

```json
{
  "attachments": [
    {
      "type": "file_snippet",
      "path": "src/db/migrations/drop-users.sql",
      "lines": "1-15"
    },
    {
      "type": "conversation_excerpt",
      "content": "User said: 'Please remove the old users table'"
    },
    {
      "type": "url",
      "url": "https://github.com/org/repo/issues/123"
    }
  ]
}
```

Reviewer sees full context, not just the command and a text explanation.

### 5. Agent Mail Integration

Since MCP Agent Mail already exists in this ecosystem, integrate:

- When request created → send agent mail to "SLB-Reviews" thread
- Reviewers can check their inbox for pending reviews
- Approval/rejection can flow through agent mail
- Creates unified coordination channel

### 6. Desktop Notifications

For CRITICAL tier commands:
- macOS: `osascript` notification
- Linux: `notify-send`
- Windows: PowerShell toast

Human gets alerted even if not watching terminals.

### 7. Learning Mode / Analytics

Track historical patterns:
- Which commands get approved vs rejected
- Which approved commands caused subsequent problems
- Which agents have high rejection rates
- Time-to-approval metrics

Surface insights in TUI dashboard. Over time, can suggest pattern refinements.

### 8. Emergency Override

Human can bypass the system when needed:

```bash
slb emergency-execute "rm -rf /tmp/stuck-process"
```

Logs extensively, requires explicit acknowledgment, but doesn't block on agent approval.

### 9. Command Templates (Allowlists)

Pre-approved command patterns that skip review:

```toml
[templates.safe]
# These patterns never need review
patterns = [
  "rm *.log",
  "rm *.tmp",
  "kubectl delete pod",      # Pods are ephemeral
  "git stash",
  "npm cache clean",
]
```

### 10. Conflict Resolution

When reviewers disagree (one approves, one rejects):

- Default: First response wins (for speed)
- Configurable: Require N approvals with 0 rejections
- Configurable: Human breaks ties
- Always: Log the disagreement for audit

---

## Technical Architecture

### Language & Runtime

**Primary**: Go 1.25 with Charmbracelet ecosystem

**Rationale** (matching NTM's proven architecture):
- **Bubble Tea** (bubbletea): Elm-architecture TUI framework with excellent composability
- **Bubbles**: Pre-built components (textinput, list, viewport, spinner, progress, etc.)
- **Lip Gloss**: CSS-like terminal styling with gradients, borders, padding
- **Glamour**: Markdown rendering for rich help text and request details
- **Cobra**: Industry-standard CLI framework with excellent completion support
- **fsnotify**: Cross-platform file system watching (inotify/FSEvents/etc.)
- **TOML** (BurntSushi/toml): Human-friendly configuration
- Compiles to single static binary - no runtime dependencies
- Excellent cross-platform support (Linux, macOS, Windows)
- NTM proves this stack produces beautiful, performant TUIs

**Key Libraries**:
```go
require (
    // Charmbracelet ecosystem (TUI excellence)
    github.com/charmbracelet/bubbletea v0.25.0    // Elm-architecture TUI framework
    github.com/charmbracelet/bubbles v0.18.0      // Pre-built components
    github.com/charmbracelet/lipgloss v1.1.1      // CSS-like styling
    github.com/charmbracelet/glamour v0.10.0      // Markdown rendering
    github.com/charmbracelet/huh v0.3.0           // Beautiful forms/prompts
    github.com/charmbracelet/log v0.3.1           // Structured colorful logging

    // CLI framework
    github.com/spf13/cobra v1.8.0                 // Industry-standard CLI
    github.com/spf13/viper v1.18.0                // Config management

    // Terminal utilities
    github.com/muesli/termenv v0.16.0             // Terminal detection
    github.com/muesli/reflow v0.3.0               // Text wrapping
    github.com/mattn/go-runewidth v0.0.16         // Unicode width handling
    github.com/mattn/go-isatty v0.0.20            // TTY detection

    // CLI output formatting
    github.com/jedib0t/go-pretty/v6 v6.5.0        // Beautiful tables, lists
    github.com/fatih/color v1.16.0                // Colored output (CLI mode)

    // Database
    modernc.org/sqlite v1.29.0                    // Pure Go SQLite (no cgo!)

    // File watching
    github.com/fsnotify/fsnotify v1.9.0           // Cross-platform file events

    // Utilities
    github.com/google/uuid v1.6.0                 // UUID generation
    github.com/samber/lo v1.39.0                  // Lodash-like utilities
    github.com/hashicorp/go-multierror v1.1.1     // Error aggregation
    github.com/sourcegraph/conc v0.3.0            // Structured concurrency

    // Configuration
    github.com/BurntSushi/toml v1.3.2             // TOML parsing
)
```

**Why these libraries**:
- **huh**: Beautiful interactive forms for TUI approve/reject dialogs
- **log**: Structured logging for daemon with pretty terminal output
- **go-pretty**: Gorgeous ASCII tables for CLI `slb pending`, `slb history`
- **lo**: Reduces boilerplate for slice/map operations (Filter, Map, Contains, etc.)
- **conc**: Clean goroutine management for daemon watchers
- **modernc.org/sqlite**: Pure Go, no cgo = simpler cross-compilation

**Visual Features** (inherited from NTM patterns):
- Catppuccin color themes (mocha, macchiato, latte, nord)
- Nerd Font icons with Unicode/ASCII fallbacks
- Animated gradients and shimmer effects
- Responsive layouts adapting to terminal width
- Mouse support alongside keyboard navigation

### Project Structure (Go/NTM-style)

```
slb/
├── cmd/
│   └── slb/
│       └── main.go                 # Entry point
│
├── internal/
│   ├── cli/                        # Cobra commands
│   │   ├── root.go                 # Root command and global flags
│   │   ├── init.go                 # slb init
│   │   ├── daemon.go               # slb daemon start/stop/status
│   │   ├── session.go              # slb session start/end/list
│   │   ├── request.go              # slb request
│   │   ├── review.go               # slb review/approve/reject
│   │   ├── execute.go              # slb execute
│   │   ├── pending.go              # slb pending
│   │   ├── history.go              # slb history
│   │   ├── config.go               # slb config
│   │   ├── patterns.go             # slb patterns
│   │   ├── watch.go                # slb watch
│   │   ├── emergency.go            # slb emergency-execute
│   │   ├── tui.go                  # slb tui (launches dashboard)
│   │   └── help.go                 # Colorized help rendering
│   │
│   ├── daemon/
│   │   ├── daemon.go               # Daemon lifecycle management
│   │   ├── watcher.go              # fsnotify-based file watcher
│   │   ├── executor.go             # Command execution with capture
│   │   ├── ipc.go                  # Unix socket server
│   │   └── notifications.go        # Desktop notifications
│   │
│   ├── db/
│   │   ├── db.go                   # SQLite connection management
│   │   ├── schema.go               # Schema definitions + migrations
│   │   ├── requests.go             # Request CRUD operations
│   │   ├── reviews.go              # Review CRUD operations
│   │   ├── sessions.go             # Session CRUD operations
│   │   └── fts.go                  # Full-text search queries
│   │
│   ├── core/
│   │   ├── request.go              # Request creation/validation
│   │   ├── review.go               # Review logic
│   │   ├── patterns.go             # Regex pattern matching
│   │   ├── dryrun.go               # Pre-flight dry run execution
│   │   ├── rollback.go             # State capture for rollback
│   │   ├── session.go              # Agent session management
│   │   ├── statemachine.go         # Request state transitions
│   │   └── signature.go            # HMAC signing for reviews
│   │
│   ├── tui/
│   │   ├── dashboard/
│   │   │   ├── dashboard.go        # Main dashboard model
│   │   │   ├── panels/
│   │   │   │   ├── pending.go      # Pending requests panel
│   │   │   │   ├── sessions.go     # Active sessions panel
│   │   │   │   ├── recent.go       # Recent activity panel
│   │   │   │   └── stats.go        # Statistics panel
│   │   │   └── keybindings.go      # Keyboard handlers
│   │   │
│   │   ├── request/
│   │   │   ├── detail.go           # Request detail view
│   │   │   ├── approve.go          # Approval form
│   │   │   └── reject.go           # Rejection form
│   │   │
│   │   ├── history/
│   │   │   ├── browser.go          # History browser with FTS
│   │   │   └── filters.go          # Filter UI
│   │   │
│   │   ├── components/
│   │   │   ├── commandbox.go       # Syntax-highlighted command display
│   │   │   ├── statusbadge.go      # Status indicators
│   │   │   ├── riskindicator.go    # CRITICAL/DANGEROUS/CAUTION badges
│   │   │   ├── agentcard.go        # Agent info card
│   │   │   ├── timeline.go         # Request timeline
│   │   │   └── spinner.go          # Loading spinners
│   │   │
│   │   ├── icons/
│   │   │   └── icons.go            # Nerd/Unicode/ASCII icon sets
│   │   │
│   │   ├── styles/
│   │   │   ├── styles.go           # Lip Gloss style definitions
│   │   │   ├── gradients.go        # Animated gradient text
│   │   │   └── shimmer.go          # Shimmer/glow effects
│   │   │
│   │   └── theme/
│   │       └── theme.go            # Catppuccin theme definitions
│   │
│   ├── git/
│   │   ├── repo.go                 # Git operations for history repo
│   │   └── commits.go              # Commit formatting
│   │
│   ├── config/
│   │   ├── config.go               # Config struct definitions
│   │   ├── defaults.go             # Default configuration
│   │   ├── loader.go               # TOML loading (project + user)
│   │   └── patterns.go             # Default dangerous patterns
│   │
│   ├── integrations/
│   │   ├── agentmail.go            # MCP Agent Mail integration
│   │   ├── claudehooks.go          # Claude Code hooks generation
│   │   └── cursor.go               # Cursor rules generation
│   │
│   ├── output/
│   │   ├── json.go                 # JSON output formatting
│   │   ├── table.go                # go-pretty table formatting
│   │   └── format.go               # Output mode detection (--json vs human)
│   │
│   └── utils/
│       ├── ids.go                  # UUID generation
│       ├── time.go                 # Timestamp handling
│       └── platform.go             # Cross-platform utilities
│
├── scripts/
│   └── install.sh                  # One-line installer
│
├── .github/
│   └── workflows/
│       ├── ci.yml                  # Lint, test, build
│       └── release.yml             # GoReleaser
│
├── go.mod
├── go.sum
├── .goreleaser.yaml
├── Makefile
└── README.md
```

### State Directories

**Project-level** (`.slb/` in project root):
```
.slb/
├── pending/                    # Active pending requests
│   └── req-<uuid>.json
├── sessions/                   # Active agent sessions
│   └── <agent-name>.json
├── rollback/                   # Captured state for rollback
│   └── req-<uuid>/
│       └── <captured-files>
├── processed/                  # Recently processed (for local review)
│   └── <date>/
│       └── req-<uuid>.json
└── config.toml                 # Project-specific config overrides
```

**User-level** (`~/.slb/`):
```
~/.slb/
├── config.toml                 # User configuration
├── history.db                  # SQLite database
├── history_git/                # Git repository for audit trail
│   ├── .git/
│   └── requests/
│       └── <year>/
│           └── <month>/
│               └── req-<uuid>.md
└── sessions/                   # Cross-project session info
```

---

## Database Schema

### SQLite Tables

```sql
-- Agent sessions
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,              -- UUID
  agent_name TEXT NOT NULL,         -- e.g., "GreenLake"
  program TEXT NOT NULL,            -- e.g., "claude-code", "codex-cli"
  model TEXT NOT NULL,              -- e.g., "opus-4.5", "gpt-5.1-codex"
  project_path TEXT NOT NULL,       -- Absolute path to project
  session_key TEXT NOT NULL,        -- HMAC key for signing
  started_at TEXT NOT NULL,         -- ISO 8601
  last_active_at TEXT NOT NULL,
  ended_at TEXT,                    -- NULL if still active
  UNIQUE(agent_name, project_path, ended_at)
);

-- Command requests
CREATE TABLE requests (
  id TEXT PRIMARY KEY,              -- UUID
  project_path TEXT NOT NULL,
  command TEXT NOT NULL,
  risk_tier TEXT NOT NULL,          -- 'critical', 'dangerous', 'caution'

  -- Requestor info
  requestor_session_id TEXT NOT NULL REFERENCES sessions(id),
  requestor_agent TEXT NOT NULL,
  requestor_model TEXT NOT NULL,

  -- Justification (structured)
  reason TEXT NOT NULL,             -- Why run this command?
  expected_effect TEXT NOT NULL,    -- What will happen?
  goal TEXT NOT NULL,               -- What are we trying to achieve?
  safety_argument TEXT NOT NULL,    -- Why is this safe/reversible?

  -- Dry run results (if applicable)
  dry_run_output TEXT,
  dry_run_command TEXT,

  -- Attachments (JSON array)
  attachments TEXT,                 -- JSON: [{type, content, ...}]

  -- State
  status TEXT NOT NULL DEFAULT 'pending',
    -- 'pending', 'approved', 'rejected', 'executed',
    -- 'execution_failed', 'cancelled', 'timeout', 'escalated'
  min_approvals INTEGER NOT NULL DEFAULT 2,
  require_different_model INTEGER NOT NULL DEFAULT 0,

  -- Execution results
  executed_at TEXT,
  execution_output TEXT,
  execution_exit_code INTEGER,

  -- Rollback info
  rollback_path TEXT,               -- Path to captured state
  rolled_back_at TEXT,

  -- Timestamps
  created_at TEXT NOT NULL,
  resolved_at TEXT,                 -- When approved/rejected/etc
  expires_at TEXT,                  -- Auto-timeout deadline

  -- Indexes
  INDEX idx_requests_status (status),
  INDEX idx_requests_project (project_path),
  INDEX idx_requests_created (created_at DESC)
);

-- Reviews (approvals and rejections)
CREATE TABLE reviews (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL REFERENCES requests(id),

  -- Reviewer info
  reviewer_session_id TEXT NOT NULL REFERENCES sessions(id),
  reviewer_agent TEXT NOT NULL,
  reviewer_model TEXT NOT NULL,

  -- Decision
  decision TEXT NOT NULL,           -- 'approve' or 'reject'
  signature TEXT NOT NULL,          -- HMAC signature with session key

  -- Structured response to requestor's justification
  reason_response TEXT,
  effect_response TEXT,
  goal_response TEXT,
  safety_response TEXT,

  -- Additional comments
  comments TEXT,

  created_at TEXT NOT NULL,

  -- Prevent duplicate reviews
  UNIQUE(request_id, reviewer_session_id)
);

-- Full-text search
CREATE VIRTUAL TABLE requests_fts USING fts5(
  command,
  reason,
  expected_effect,
  goal,
  safety_argument,
  content='requests',
  content_rowid='rowid'
);

-- Analytics/learning
CREATE TABLE execution_outcomes (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL REFERENCES requests(id),

  -- Outcome assessment
  caused_problems INTEGER NOT NULL DEFAULT 0,
  problem_description TEXT,

  -- Human feedback
  human_rating INTEGER,             -- 1-5 scale
  human_notes TEXT,

  created_at TEXT NOT NULL
);

-- Pattern management (agents can ADD, only humans can REMOVE)
CREATE TABLE pattern_changes (
  id TEXT PRIMARY KEY,
  change_type TEXT NOT NULL,        -- 'add', 'remove_request', 'remove_approved', 'suggest'
  tier TEXT NOT NULL,               -- 'critical', 'dangerous', 'caution', 'safe'
  pattern TEXT NOT NULL,
  reason TEXT NOT NULL,             -- Why add/remove this pattern

  -- Who made the change
  agent_session_id TEXT REFERENCES sessions(id),
  agent_name TEXT,

  -- For removal requests
  status TEXT,                      -- 'pending', 'approved', 'rejected' (for remove_request)
  reviewed_by TEXT,                 -- Human who approved/rejected
  reviewed_at TEXT,

  created_at TEXT NOT NULL,

  INDEX idx_pattern_changes_status (status),
  INDEX idx_pattern_changes_type (change_type)
);

-- Track which patterns are agent-added vs built-in
CREATE TABLE custom_patterns (
  id TEXT PRIMARY KEY,
  tier TEXT NOT NULL,
  pattern TEXT NOT NULL,
  reason TEXT,
  source TEXT NOT NULL,             -- 'builtin', 'agent', 'human', 'suggested'
  added_by TEXT,                    -- Agent name or 'human'
  added_at TEXT NOT NULL,
  removed_at TEXT,                  -- NULL if still active

  UNIQUE(tier, pattern)
);
```

### Go Types

```go
package db

import "time"

type RiskTier string

const (
    RiskCritical  RiskTier = "critical"
    RiskDangerous RiskTier = "dangerous"
    RiskCaution   RiskTier = "caution"
)

type RequestStatus string

const (
    StatusPending         RequestStatus = "pending"
    StatusApproved        RequestStatus = "approved"
    StatusRejected        RequestStatus = "rejected"
    StatusExecuted        RequestStatus = "executed"
    StatusExecutionFailed RequestStatus = "execution_failed"
    StatusCancelled       RequestStatus = "cancelled"
    StatusTimeout         RequestStatus = "timeout"
    StatusEscalated       RequestStatus = "escalated"
)

type Session struct {
    ID           string    `json:"id"`
    AgentName    string    `json:"agent_name"`
    Program      string    `json:"program"`      // claude-code, codex-cli, cursor, etc.
    Model        string    `json:"model"`        // opus-4.5, gpt-5.1-codex, etc.
    ProjectPath  string    `json:"project_path"`
    SessionKey   string    `json:"-"`            // HMAC key, not serialized
    StartedAt    time.Time `json:"started_at"`
    LastActiveAt time.Time `json:"last_active_at"`
    EndedAt      *time.Time `json:"ended_at,omitempty"`
}

type Requestor struct {
    SessionID string `json:"session_id"`
    AgentName string `json:"agent_name"`
    Model     string `json:"model"`
}

type Justification struct {
    Reason         string `json:"reason"`
    ExpectedEffect string `json:"expected_effect"`
    Goal           string `json:"goal"`
    SafetyArgument string `json:"safety_argument"`
}

type DryRun struct {
    Command string `json:"command"`
    Output  string `json:"output"`
}

type Execution struct {
    ExecutedAt time.Time `json:"executed_at"`
    Output     string    `json:"output"`
    ExitCode   int       `json:"exit_code"`
}

type Rollback struct {
    Path         string     `json:"path"`
    RolledBackAt *time.Time `json:"rolled_back_at,omitempty"`
}

type Attachment struct {
    Type     string            `json:"type"`     // file_snippet, conversation_excerpt, url, image
    Content  string            `json:"content"`
    Metadata map[string]any    `json:"metadata,omitempty"`
}

type Request struct {
    ID          string        `json:"id"`
    ProjectPath string        `json:"project_path"`
    Command     string        `json:"command"`
    RiskTier    RiskTier      `json:"risk_tier"`

    Requestor     Requestor     `json:"requestor"`
    Justification Justification `json:"justification"`

    DryRun      *DryRun      `json:"dry_run,omitempty"`
    Attachments []Attachment `json:"attachments"`

    Status               RequestStatus `json:"status"`
    MinApprovals         int           `json:"min_approvals"`
    RequireDifferentModel bool         `json:"require_different_model"`

    Execution *Execution `json:"execution,omitempty"`
    Rollback  *Rollback  `json:"rollback,omitempty"`

    CreatedAt  time.Time  `json:"created_at"`
    ResolvedAt *time.Time `json:"resolved_at,omitempty"`
    ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

type Reviewer struct {
    SessionID string `json:"session_id"`
    AgentName string `json:"agent_name"`
    Model     string `json:"model"`
}

type ReviewResponses struct {
    Reason string `json:"reason"`
    Effect string `json:"effect"`
    Goal   string `json:"goal"`
    Safety string `json:"safety"`
}

type Review struct {
    ID        string   `json:"id"`
    RequestID string   `json:"request_id"`
    Reviewer  Reviewer `json:"reviewer"`

    Decision  string `json:"decision"` // "approve" or "reject"
    Signature string `json:"signature"`

    Responses ReviewResponses `json:"responses"`
    Comments  string          `json:"comments,omitempty"`

    CreatedAt time.Time `json:"created_at"`
}
```

---

## Request Lifecycle State Machine

```
                                    ┌─────────────┐
                                    │  CANCELLED  │
                                    └─────────────┘
                                          ▲
                                          │ cancel
                                          │
┌─────────┐      ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
│ CREATED │ ───▶ │   PENDING   │───▶│  APPROVED   │───▶│  EXECUTED   │
└─────────┘      └─────────────┘    └─────────────┘    └─────────────┘
                       │                                      │
                       │                                      ▼
                       │                              ┌───────────────────┐
                       │ reject                       │ EXECUTION_FAILED  │
                       ▼                              └───────────────────┘
                 ┌─────────────┐
                 │  REJECTED   │
                 └─────────────┘
                       │
                       │ timeout
                       ▼
                 ┌─────────────┐    ┌─────────────┐
                 │   TIMEOUT   │───▶│  ESCALATED  │ (human notified)
                 └─────────────┘    └─────────────┘
```

### State Transitions

| From | To | Trigger |
|------|-----|---------|
| (new) | PENDING | `slb request` creates request |
| PENDING | APPROVED | Required approvals received |
| PENDING | REJECTED | Any rejection received |
| PENDING | CANCELLED | Requestor cancels |
| PENDING | TIMEOUT | Expiry time reached |
| TIMEOUT | ESCALATED | Human notification sent |
| APPROVED | EXECUTED | `slb execute` runs command |
| APPROVED | CANCELLED | Requestor decides not to execute |
| EXECUTED | - | Terminal state |
| EXECUTION_FAILED | - | Terminal state |
| REJECTED | - | Terminal state |

---

## CLI Commands

### Core Commands

```bash
# Initialize slb in a project
slb init [--force]
  Creates .slb/ directory structure
  Adds .slb to .gitignore
  Generates project config.toml

# Version info
slb version [--json]
  Shows version, build info, config paths

# Daemon management
slb daemon start [--foreground]
slb daemon stop
slb daemon status
slb daemon logs [--follow] [--lines N]

# Session management (for agents)
slb session start --agent <name> --program <prog> --model <model>
  Returns: session ID and key
  Alias: -a for --agent, -p for --program, -m for --model

slb session end [--session-id <id>]
  Alias: -s for --session-id (used globally for all commands)

slb session list [--project <path>] [--json]
slb session heartbeat --session-id <id>
```

**Global flag aliases** (apply to all commands):
- `-s` → `--session-id`
- `-j` → `--json`
- `-p` → `--project`

### Request Commands

```bash
# Submit a command for approval (primary command for agents)
slb request "<command>" \
  --reason "Why I need to run this" \
  --expected-effect "What will happen" \
  --goal "What I'm trying to achieve" \
  --safety "Why this is safe/reversible" \
  [--attach-file <path>:<lines>] \
  [--attach-context "<text>"] \
  [--session-id <id>] \
  [--wait]                        # Block until approved/rejected
  [--timeout <seconds>]

  Returns: request ID

# Check request status
slb status <request-id>
  Returns: current status, reviews, etc.

# List pending requests
slb pending [--project <path>] [--all-projects]
  Returns: list of pending requests

# Cancel own request
slb cancel <request-id> --session-id <id>
```

### Review Commands

```bash
# Review a pending request (full details)
slb review <request-id>
  Shows: command, justification, dry-run output, attachments

# Approve a request
slb approve <request-id> \
  --session-id <id> \
  [--reason-response "..."] \
  [--effect-response "..."] \
  [--goal-response "..."] \
  [--safety-response "..."] \
  [--comment "Additional notes"]

# Reject a request
slb reject <request-id> \
  --session-id <id> \
  --reason "Why this should not run" \
  [--reason-response "..."] \
  [--effect-response "..."] \
  [--goal-response "..."] \
  [--safety-response "..."]
```

### Execution Commands

```bash
# Execute an approved request
slb execute <request-id> [--session-id <id>]
  Runs the command, captures output
  Returns: exit code, stdout, stderr

# Emergency execute (human override, bypasses approval)
slb emergency-execute "<command>" \
  --reason "Why this can't wait" \
  [--capture-rollback]

  Requires: interactive confirmation
  Logs: extensively for audit
```

### History & Search

```bash
# Search history
slb history \
  [--query "<fts query>"] \
  [--status <status>] \
  [--project <path>] \
  [--agent <name>] \
  [--since <date>] \
  [--until <date>] \
  [--limit N] \
  [--json]

# View specific historical request
slb show <request-id> [--json]

# Rollback an executed command
slb rollback <request-id>
  Requires: rollback data was captured
```

### Configuration

```bash
# View/edit configuration
slb config [--global | --project]
slb config get <key>
slb config set <key> <value>
slb config edit             # Opens in $EDITOR

# Pattern management
slb patterns list [--tier <tier>] [-j]
slb patterns test "<command>"    # Shows which tier it matches
  Alias: slb check "<command>"   # Shorter version for agents
  --exit-code                    # Return non-zero if approval needed (for hooks)

# Adding patterns (agents CAN do this freely)
slb patterns add --tier <tier> "<pattern>" [--reason "why this is dangerous"]
  Agents can add patterns to protect against mistakes they've seen
  Added patterns are logged with agent name and reason
  Example: slb patterns add --tier critical "^kubectl drain" --reason "Can evict all pods"

# Removing patterns (requires human approval!)
slb patterns remove "<pattern>"
  BLOCKED for agents - prints message to use TUI or get human approval
  Returns exit code 1 with JSON: {"error": "pattern_removal_requires_human", "use": "slb tui"}

slb patterns request-removal "<pattern>" --reason "why this should be safe"
  Agents can REQUEST removal, creates a pending removal request
  Human reviews in TUI and approves/rejects
  Removal requests shown in TUI dashboard alongside command requests

# Pattern suggestions (for agents to propose new patterns)
slb patterns suggest --tier <tier> "<pattern>" --reason "..."
  Like add, but marks as "suggested" for human review
  Human can promote to permanent or dismiss
```

### Watch Mode (for reviewing agents)

```bash
# Watch for pending requests and prompt for review
slb watch \
  [--project <path>] \
  [--session-id <id>] \
  [--auto-approve-caution]    # Auto-approve CAUTION tier
```

### TUI Dashboard (Human-Only Interface)

```bash
# Launch full TUI dashboard - the ONLY interactive/human interface
slb tui
slb dashboard                 # Alias
```

### Base Command: Quick Reference Card

The entire CLI is designed for agent (robot) usage. Running `slb` with no arguments prints a colorful quick reference card using lipgloss styling:

```go
// internal/cli/root.go - when no subcommand provided
func printQuickRef() {
    // Colors (Catppuccin Mocha)
    title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#cba6f7")) // Mauve
    section := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#89b4fa")) // Blue
    cmd := lipgloss.NewStyle().Foreground(lipgloss.Color("#a6e3a1")) // Green
    flag := lipgloss.NewStyle().Foreground(lipgloss.Color("#f9e2af")) // Yellow
    comment := lipgloss.NewStyle().Foreground(lipgloss.Color("#6c7086")) // Overlay0
    tier := map[string]lipgloss.Style{
        "critical":  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#f38ba8")), // Red
        "dangerous": lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#fab387")), // Peach
        "caution":   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#f9e2af")), // Yellow
    }
    // ... render card with box drawing
}
```

**Output** (rendered with colors):

```
┌─────────────────────────────────────────────────────────────────────────┐
│  ⚡ SLB — Simultaneous Launch Button                           v1.0.0  │
│     Two-agent approval for dangerous commands                          │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  START SESSION (once per agent):                                        │
│    slb session start -a <Name> -p claude-code -m opus-4.5 -j           │
│    → Save session_id from JSON output                                   │
│                                                                         │
│  BEFORE DANGEROUS COMMANDS:                                             │
│    slb check "rm -rf ./build"                    # Need approval?       │
│    slb request "..." -s $SID \                                          │
│        --reason "..." --expected-effect "..."                           │
│        --goal "..." --safety "..."                                      │
│    slb status $REQ --wait -j                     # Block til decision   │
│    slb execute $REQ -j                           # Run if approved      │
│                                                                         │
│  AS REVIEWER (check periodically!):                                     │
│    slb pending -j                                # List pending         │
│    slb review <id> -j                            # Full details         │
│    slb approve <id> -s $SID                      # Sign off             │
│    slb reject <id> -s $SID --reason "..."        # Block it             │
│                                                                         │
│  ADD NEW DANGEROUS PATTERNS (agents can do this!):                      │
│    slb patterns add --tier critical "^kubectl drain" --reason "..."     │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│  TIERS: 🔴 CRITICAL (2 approvals)  🟠 DANGEROUS (1)  🟡 CAUTION (auto)  │
│  FLAGS: -s/--session-id   -j/--json   -p/--project                      │
│  HUMAN: slb tui                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

**Implementation notes**:
- Box drawing uses lipgloss borders with rounded corners
- Title uses gradient text effect (mauve → blue)
- Commands are syntax-highlighted (green for commands, yellow for flags)
- Tier badges are colored: 🔴 red, 🟠 orange, 🟡 yellow
- Responsive: adjusts to terminal width (min 72 cols, max 100)
- Falls back to ASCII on terminals without Unicode support

**Design Philosophy**:
- Every command is CLI-first, non-interactive
- All commands support `--json` for structured output
- No separate "robot mode" - the CLI IS the robot interface
- TUI dashboard (`slb tui`) is the only human-facing interface
- Agents should never need to parse human-formatted output

---

## TUI Design (Human Dashboard Only)

### Layout (Dashboard View)

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│  SLB — Simultaneous Launch Button                            ⚡ 3 agents online │
├────────────────────────────┬────────────────────────────────────────────────────┤
│  ACTIVE AGENTS             │  PENDING REQUESTS (2)                              │
│  ─────────────             │  ─────────────────────                              │
│  🟢 GreenLake              │  ┌────────────────────────────────────────────────┐│
│     claude-code opus-4.5   │  │ 🔴 CRITICAL  req-a1b2  2m ago                  ││
│     Last: 30s ago          │  │ kubectl delete node worker-3                    ││
│                            │  │ By: BlueDog (codex gpt-5.1)                     ││
│  🟢 BlueDog                │  │ "Need to remove failing node for cluster..."   ││
│     codex-cli gpt-5.1      │  │                                                 ││
│     Last: 45s ago          │  │ [R]eview  [A]pprove  [X]Reject                 ││
│                            │  └────────────────────────────────────────────────┘│
│  🟢 RedStone               │                                                    │
│     cursor claude-sonnet   │  ┌────────────────────────────────────────────────┐│
│     Last: 2m ago           │  │ 🟡 DANGEROUS  req-c3d4  5m ago                 ││
│                            │  │ git reset --hard HEAD~3                         ││
│                            │  │ By: GreenLake (claude-code opus-4.5)            ││
│                            │  │ "Reverting broken commits from..."              ││
├────────────────────────────┤  │                                                 ││
│  STATS (24h)               │  │ [R]eview  [A]pprove  [X]Reject                 ││
│  ──────────                │  └────────────────────────────────────────────────┘│
│  Approved:     23          │                                                    │
│  Rejected:      4          │                                                    │
│  Executed:     21          │                                                    │
│  Avg response: 45s         ├────────────────────────────────────────────────────┤
│                            │  RECENT ACTIVITY                                   │
│                            │  ───────────────                                   │
│                            │  ✅ req-x9y0 rm -rf ./tmp  EXECUTED  10m ago       │
│                            │  ❌ req-w8v7 DROP TABLE... REJECTED  15m ago       │
│                            │  ✅ req-u6t5 git clean... EXECUTED  20m ago        │
│                            │  ✅ req-s4r3 kubectl...   EXECUTED  25m ago        │
└────────────────────────────┴────────────────────────────────────────────────────┘
│ [P]ending  [H]istory  [S]essions  [C]onfig  [?]Help  [Q]uit                     │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Request Detail View

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│  REQUEST DETAIL — req-a1b2c3d4                                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  COMMAND                                                                        │
│  ┌─────────────────────────────────────────────────────────────────────────────┐│
│  │ kubectl delete node worker-3                                                ││
│  └─────────────────────────────────────────────────────────────────────────────┘│
│                                                                                 │
│  RISK: 🔴 CRITICAL          STATUS: ⏳ PENDING (0/2 approvals)                  │
│                                                                                 │
│  REQUESTOR                                                                      │
│  ──────────                                                                     │
│  Agent: BlueDog                                                                 │
│  Program: codex-cli                                                             │
│  Model: gpt-5.1-codex                                                           │
│  Submitted: 2025-12-13 14:32:05 (2 minutes ago)                                 │
│  Expires: 2025-12-13 15:02:05 (28 minutes remaining)                            │
│                                                                                 │
│  JUSTIFICATION                                                                  │
│  ─────────────                                                                  │
│  Reason:                                                                        │
│    Worker-3 has been in NotReady state for 15 minutes after a kernel panic.    │
│    Pods have been rescheduled to other nodes. Removing the dead node cleanly.   │
│                                                                                 │
│  Expected Effect:                                                               │
│    Node worker-3 will be removed from the cluster. All resources that were     │
│    running on it have already been evicted. The cloud provider will not be     │
│    notified (no --cascade).                                                     │
│                                                                                 │
│  Goal:                                                                          │
│    Clean up cluster state by removing reference to the failed node.            │
│                                                                                 │
│  Safety Argument:                                                               │
│    Node is already dead and not serving traffic. Removal is cosmetic cleanup.  │
│    Can re-add node later by re-provisioning if needed.                         │
│                                                                                 │
│  DRY RUN OUTPUT                                                                 │
│  ──────────────                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────────┐│
│  │ $ kubectl delete node worker-3 --dry-run=client                             ││
│  │ node "worker-3" deleted (dry run)                                           ││
│  └─────────────────────────────────────────────────────────────────────────────┘│
│                                                                                 │
│  ATTACHMENTS (1)                                                                │
│  ───────────────                                                                │
│  📎 kubectl_get_nodes.txt (click to expand)                                     │
│                                                                                 │
│  REVIEWS (0)                                                                    │
│  ──────────                                                                     │
│  No reviews yet.                                                                │
│                                                                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│ [A]pprove  [X]Reject  [C]opy command  [B]ack                                   │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### History Browser View

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│  HISTORY                                                    Search: terraform   │
├─────────────────────────────────────────────────────────────────────────────────┤
│  Filter: [All ▼]  Status: [All ▼]  Agent: [All ▼]  Since: [7 days ▼]           │
├─────────────────────────────────────────────────────────────────────────────────┤
│  ID       │ Command                      │ Status   │ Agent     │ Time         │
│  ─────────┼──────────────────────────────┼──────────┼───────────┼────────────  │
│  req-a1b2 │ terraform destroy -target... │ ✅ EXEC  │ GreenLake │ 2h ago       │
│  req-c3d4 │ terraform apply -auto-app... │ ✅ EXEC  │ BlueDog   │ 3h ago       │
│  req-e5f6 │ terraform destroy            │ ❌ REJ   │ RedStone  │ 1d ago       │
│  req-g7h8 │ terraform state rm module... │ ✅ EXEC  │ GreenLake │ 2d ago       │
│  req-i9j0 │ terraform import aws_s3...   │ ✅ EXEC  │ BlueDog   │ 3d ago       │
│           │                              │          │           │              │
│           │                              │          │           │              │
│           │                              │          │           │              │
├─────────────────────────────────────────────────────────────────────────────────┤
│  Showing 5 of 23 results                                      Page 1/5  < >    │
├─────────────────────────────────────────────────────────────────────────────────┤
│ [Enter] View detail  [/] Search  [F]ilter  [E]xport  [B]ack                    │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Visual Design Principles

1. **Information density**: Show key info at a glance, details on demand
2. **Color coding**:
   - 🔴 Red: CRITICAL risk, rejected requests
   - 🟡 Yellow: DANGEROUS risk, pending
   - 🟢 Green: approved, executed, safe
   - 🔵 Blue: informational
3. **Keyboard-first**: All actions have single-key shortcuts
4. **Responsive**: Adapts to terminal width (80 col minimum, scales to ultrawide)
5. **Real-time updates**: Dashboard refreshes as state changes
6. **Mouse support**: Optional, keyboard always works

---

## Configuration System

### Configuration Hierarchy

1. **Built-in defaults** (hardcoded sensible defaults)
2. **User config** (`~/.slb/config.toml`)
3. **Project config** (`.slb/config.toml`)
4. **Environment variables** (`SLB_*`)
5. **Command-line flags** (highest priority)

### Configuration Schema

```toml
# ~/.slb/config.toml or .slb/config.toml

[general]
# Minimum approvals required (can be overridden per-tier)
min_approvals = 2

# Require approver to be different model than requestor
require_different_model = false

# Timeout for pending requests (seconds)
request_timeout = 1800  # 30 minutes

# What to do when request times out
timeout_action = "escalate"  # "escalate" | "auto_reject" | "auto_approve_warn"

# Enable dry-run pre-flight for supported commands
enable_dry_run = true

# Capture state for rollback (may use disk space)
enable_rollback_capture = true
max_rollback_size_mb = 100

[daemon]
# Use file watcher instead of polling
use_file_watcher = true

# IPC socket path (for fast agent communication)
ipc_socket = "/tmp/slb-{project-hash}.sock"

# Log level
log_level = "info"

[notifications]
# Desktop notifications for CRITICAL pending requests
desktop_enabled = true

# Notify after this many seconds pending
desktop_delay_seconds = 60

# Webhook URL for notifications (Slack, Discord, etc.)
webhook_url = ""

# Email notifications (requires SMTP config)
email_enabled = false

[history]
# SQLite database location
database_path = "~/.slb/history.db"

# Git repository for audit trail
git_repo_path = "~/.slb/history_git"

# Retention period for history (days, 0 = forever)
retention_days = 365

# Sync to git after each request
auto_git_commit = true

[patterns]
# Risk tiers: critical, dangerous, caution
# Patterns are regex (case-insensitive by default)

[patterns.critical]
# These ALWAYS require 2+ approvals from OTHER agents (not the requestor)
# So for CRITICAL: requestor + 2 reviewers = minimum 3 agents involved
min_approvals = 2
patterns = [
  "^rm\\s+-rf\\s+/(?!tmp)",           # rm -rf / (but not /tmp)
  "^rm\\s+-rf\\s+~",                  # rm -rf ~
  "DROP\\s+DATABASE",                 # SQL DROP DATABASE
  "DROP\\s+SCHEMA",
  "TRUNCATE\\s+TABLE",
  "DELETE\\s+FROM\\s+\\w+\\s*;",      # DELETE without WHERE (deletes ALL rows!)
  "DELETE\\s+FROM(?!.*WHERE)",        # DELETE without WHERE clause
  "^terraform\\s+destroy(?!.*-target)", # terraform destroy (without -target)
  "^kubectl\\s+delete\\s+(node|namespace|pv|pvc)",
  "^helm\\s+uninstall.*--all",
  "^docker\\s+system\\s+prune\\s+-a",
  "^git\\s+push.*--force(?!-with-lease)",  # force push (not with-lease)
  "^aws\\s+.*terminate-instances",
  "^gcloud.*delete.*--quiet",
]

[patterns.dangerous]
# Require 1 approval by default
min_approvals = 1
patterns = [
  "^rm\\s+-rf",                       # Any rm -rf
  "^rm\\s+-r",                        # Any rm -r
  "^git\\s+reset\\s+--hard",
  "^git\\s+clean\\s+-fd",
  "^git\\s+push.*--force-with-lease",
  "^kubectl\\s+delete",               # Any kubectl delete
  "^helm\\s+uninstall",
  "^docker\\s+rm",
  "^docker\\s+rmi",
  "^terraform\\s+destroy.*-target",   # targeted destroy
  "^terraform\\s+state\\s+rm",
  "DROP\\s+TABLE",
  "DELETE\\s+FROM.*WHERE",            # DELETE with WHERE
  "^chmod\\s+-R",
  "^chown\\s+-R",
]

[patterns.caution]
# Auto-approved after delay with logging
min_approvals = 0
auto_approve_delay_seconds = 30
patterns = [
  "^rm\\s+[^-]",                       # rm without flags (single files)
  "^git\\s+stash\\s+drop",
  "^git\\s+branch\\s+-[dD]",
  "^npm\\s+uninstall",
  "^pip\\s+uninstall",
  "^cargo\\s+remove",
]

[patterns.safe]
# These patterns SKIP review entirely
patterns = [
  "^rm\\s+.*\\.log$",
  "^rm\\s+.*\\.tmp$",
  "^rm\\s+.*\\.bak$",
  "^git\\s+stash(?!.*drop)",
  "^kubectl\\s+delete\\s+pod",        # Pods are ephemeral
  "^npm\\s+cache\\s+clean",
]

[integrations]
# Agent Mail integration
agent_mail_enabled = true
agent_mail_thread = "SLB-Reviews"

# Claude Code hooks
claude_hooks_enabled = true

[agents]
# Trusted agents that can self-approve after delay
trusted_self_approve = []
trusted_self_approve_delay_seconds = 300

# Agents that are blocked from making requests
blocked = []
```

### Default Dangerous Patterns

Organized by domain:

**File System**:
- `rm -rf`, `rm -r` (with path analysis)
- `chmod -R`, `chown -R` on sensitive paths
- Operations on `/etc`, `/usr`, `/var`, `/boot`

**Git**:
- `git reset --hard`
- `git clean -fd`
- `git push --force` (but not `--force-with-lease`)
- `git rebase` on main/master

**Kubernetes**:
- `kubectl delete node|namespace|pv|pvc`
- `kubectl delete` anything in `kube-system`
- `helm uninstall --all`

**Databases**:
- `DROP DATABASE/SCHEMA/TABLE`
- `TRUNCATE TABLE`
- `DELETE FROM` without `WHERE`

**Cloud**:
- `terraform destroy` (without -target)
- `aws * terminate-instances`
- `gcloud * delete --quiet`

**Containers**:
- `docker system prune -a`
- `docker rm -f $(docker ps -aq)`

---

## Integration Patterns

### Claude Code Hooks

Generate a `.claude/hooks.json` that intercepts dangerous commands:

```json
{
  "hooks": {
    "pre_bash": {
      "command": "slb patterns test --exit-code",
      "input": {
        "command": "${COMMAND}"
      },
      "on_block": {
        "message": "This command requires slb approval. Use: slb request \"${COMMAND}\" --reason \"...\" --expected-effect \"...\" --goal \"...\" --safety \"...\""
      }
    }
  }
}
```

The `--exit-code` flag makes `slb patterns test` return non-zero if approval is needed, triggering the hook's `on_block`.

Generate with:
```bash
slb integrations claude-hooks --install
```

### Cursor Rules

Generate `.cursorrules` section:

```markdown
## Dangerous Command Policy

Before running any command matching these patterns, you MUST use slb:

1. Check if command needs approval: `slb patterns test "<command>"`
2. If approval needed: `slb request "<command>" --reason "..." --expected-effect "..." --goal "..." --safety "..."`
3. Wait for approval: `slb status <request-id>`
4. Execute when approved: `slb execute <request-id>`

Never run dangerous commands directly.
```

### Agent Mail Integration

When a request is created, slb sends a notification via Agent Mail:

```go
// internal/integrations/agentmail.go
func (am *AgentMailClient) NotifyNewRequest(req *db.Request) error {
    importance := "normal"
    if req.RiskTier == db.RiskCritical {
        importance = "urgent"
    }

    return am.SendMessage(agentmail.Message{
        ProjectKey: req.ProjectPath,
        SenderName: "SLB-System",
        To:         []string{"SLB-Broadcast"}, // Virtual broadcast
        Subject:    fmt.Sprintf("[SLB] %s: %s", strings.ToUpper(string(req.RiskTier)), truncate(req.Command, 50)),
        BodyMD: fmt.Sprintf(`## Command Approval Request

**ID**: %s
**Risk**: %s
**Command**: ` + "`%s`" + `

### Justification
%s

### Expected Effect
%s

---
To review: ` + "`slb review %s`" + `
To approve: ` + "`slb approve %s --session-id <your-session>`",
            req.ID, req.RiskTier, req.Command,
            req.Justification.Reason, req.Justification.ExpectedEffect,
            req.ID, req.ID),
        Importance: importance,
        ThreadID:   "SLB-Reviews",
    })
}
```

---

## Agent Workflow

### For Requesting Agent

```bash
# 1. Start session (once per agent lifetime)
SESSION_JSON=$(slb session start \
  --agent "GreenLake" \
  --program "claude-code" \
  --model "opus-4.5" \
  --json)

SESSION_ID=$(echo "$SESSION_JSON" | jq -r '.sessionId')

# 2. When dangerous command needed, check pattern first
PATTERN_CHECK=$(slb patterns test "rm -rf ./build" --json)
NEEDS_APPROVAL=$(echo "$PATTERN_CHECK" | jq -r '.needsApproval')

if [ "$NEEDS_APPROVAL" = "true" ]; then
  # 3. Submit request
  REQUEST_JSON=$(slb request "rm -rf ./build" \
    --session-id "$SESSION_ID" \
    --reason "Removing stale build artifacts" \
    --expected-effect "Deletes ./build directory (~500MB)" \
    --goal "Free up disk space before next build" \
    --safety "Build directory is regenerated on next build, no source code" \
    --json)

  REQUEST_ID=$(echo "$REQUEST_JSON" | jq -r '.requestId')

  # 4. Wait for approval (with timeout)
  STATUS_JSON=$(slb status "$REQUEST_ID" --wait --timeout 300 --json)
  STATUS=$(echo "$STATUS_JSON" | jq -r '.status')

  case "$STATUS" in
    approved)
      # 5. Execute
      slb execute "$REQUEST_ID" --json
      ;;
    rejected)
      echo "Request rejected: $(echo "$STATUS_JSON" | jq -r '.reviews[0].reason')"
      # Reconsider approach...
      ;;
    timeout)
      echo "Request timed out, escalating to human..."
      ;;
  esac
fi

# 6. End session when done
slb session end --session-id "$SESSION_ID"
```

### For Reviewing Agent

```bash
# Option 1: Watch mode (blocks and prompts for each request)
slb watch --session-id "$SESSION_ID"

# Option 2: Poll and review (useful in scripts)
PENDING=$(slb pending --json)

echo "$PENDING" | jq -c '.[]' | while read -r request; do
  REQUEST_ID=$(echo "$request" | jq -r '.id')

  # Get full details
  DETAILS=$(slb review "$REQUEST_ID" --json)

  # Evaluate the request (in practice, an AI agent reads and reasons about this)
  COMMAND=$(echo "$DETAILS" | jq -r '.command')
  REASON=$(echo "$DETAILS" | jq -r '.justification.reason')

  # Example: approve if it's a build directory cleanup
  if echo "$COMMAND" | grep -q "./build"; then
    slb approve "$REQUEST_ID" \
      --session-id "$SESSION_ID" \
      --reason-response "Valid cleanup operation" \
      --effect-response "Confirmed build dir is ephemeral" \
      --goal-response "Aligns with disk space management" \
      --safety-response "Verified no source code in path"
  else
    slb reject "$REQUEST_ID" \
      --session-id "$SESSION_ID" \
      --reason "This path may contain source code, not just build artifacts"
  fi
done
```

---

## Security Considerations

### What slb IS

- **Peer review system**: Second opinion on destructive actions
- **Audit trail**: Complete record of what was proposed, approved, executed
- **Friction mechanism**: Forces deliberation before destruction
- **Learning tool**: Patterns can be refined based on outcomes

### What slb IS NOT

- **Cryptographic security**: Session keys are for identification, not authentication
- **Tamper-proof**: A malicious agent could bypass slb entirely
- **Access control**: Doesn't prevent command execution, just adds a step

### Trust Model

1. **Agents are trusted but fallible**: They follow instructions but make mistakes
2. **Honor system**: Agents are instructed to use slb, but can't be forced
3. **Defense in depth**: slb is one layer; combine with file permissions, RBAC, etc.
4. **Audit over prevention**: Focus on knowing what happened, not preventing everything

### Recommendations

1. **Always gitignore .slb/**: Prevent accidental commits of pending requests
2. **Use separate git history repo**: Keep audit trail separate from project
3. **Review history periodically**: Look for patterns of concern
4. **Set tight timeouts for CRITICAL**: Don't let dangerous requests linger
5. **Enable desktop notifications**: Human should know about CRITICAL requests

---

## Edge Cases & Error Handling

### Edge Case 1: No Other Agents Available

**Scenario**: Single agent working alone, needs dangerous command.

**Solution**: This is by design. slb requires multiple agents for peer review. If you're working solo:
1. Request times out → escalates to human (desktop notification)
2. Human uses `slb tui` to approve/reject
3. Or human uses `slb emergency-execute` with full logging

**No self-approval**: The whole point is peer review. If you're alone, get a human.

### Edge Case 2: Agent Approves Own Request

**Prevention**: Explicit check that `requestor_session_id != reviewer_session_id`

**Special case**: If configured `trusted_self_approve` and agent is trusted, allow after delay.

### Edge Case 3: Conflicting Reviews

**Scenario**: Agent A approves, Agent B rejects the same request.

**Resolution options** (configurable):
1. First response wins (speed priority)
2. Any rejection blocks (safety priority)
3. Majority vote with minimum threshold
4. Human breaks tie

**Default**: Any rejection blocks. Safety over speed.

### Edge Case 4: Stale Sessions

**Scenario**: Agent crashes without ending session.

**Solutions**:
1. Session heartbeat requirement (every 5 min)
2. Auto-expire sessions after inactivity (30 min)
3. `slb session gc` to clean up stale sessions

### Edge Case 5: Large Dry Run Output

**Scenario**: `kubectl delete` on 1000 pods produces huge output.

**Solutions**:
1. Truncate with "... and N more lines"
2. Store full output in file, show summary in request
3. Configurable max dry run output size

### Edge Case 6: Request During Daemon Downtime

**Scenario**: Request file created but daemon not running.

**Solutions**:
1. Daemon startup scans pending/ for stale requests
2. Recalculate timeouts from creation time
3. Warn if requests found that are past expiry

### Edge Case 7: Filesystem Permissions

**Scenario**: Agent can't write to .slb/pending.

**Solutions**:
1. `slb init` creates directory with appropriate permissions
2. Clear error message: "Cannot write to .slb/, check permissions"
3. Fallback to user-level pending queue

---

## Implementation Phases

### Phase 1: Core Foundation (Days 1-2)

**Goal**: Basic request/approve/execute flow works.

- [ ] Project initialization (`slb init`)
- [ ] Session management (start, end, list)
- [ ] Request creation with pattern matching
- [ ] Review, approve, reject commands
- [ ] Execute approved requests
- [ ] SQLite schema and basic queries
- [ ] File-based pending queue
- [ ] JSON output mode for all commands
- [ ] Unit tests for pattern matching
- [ ] Unit tests for state machine transitions

**Deliverable**: Can manually test request→approve→execute cycle. Core logic has test coverage.

### Phase 2: Daemon & Watching (Days 2-3)

**Goal**: Background processes work.

- [ ] Daemon with file system watcher (not polling)
- [ ] Unix socket IPC for fast communication
- [ ] State machine transitions
- [ ] Timeout handling
- [ ] Watch mode for reviewing agents
- [ ] Status command with --wait

**Deliverable**: Agents can submit and wait for approval asynchronously.

### Phase 3: TUI Dashboard (Days 3-4)

**Goal**: Beautiful, functional TUI.

- [ ] Dashboard view with agent list, pending requests
- [ ] Request detail view
- [ ] Approve/reject from TUI
- [ ] History browser with FTS search
- [ ] Real-time updates
- [ ] Keyboard navigation
- [ ] Responsive layout

**Deliverable**: Humans can monitor and intervene via TUI.

### Phase 4: Advanced Features (Days 4-5)

**Goal**: Production-ready features.

- [ ] Dry-run pre-flight for supported commands
- [ ] Rollback capture and restore
- [ ] Context attachments
- [ ] Desktop notifications
- [ ] Git history repository
- [ ] Configuration management
- [ ] Pattern test command

**Deliverable**: Full feature set for real usage.

### Phase 5: Integrations & Polish (Days 5-6)

**Goal**: Ecosystem integration.

- [ ] Claude Code hooks generator
- [ ] Agent Mail integration
- [ ] Cursor rules generator
- [ ] Emergency override
- [ ] Analytics/learning mode
- [ ] Documentation (README, --help text)
- [ ] Integration tests (full request→approve→execute flow)
- [ ] Cross-platform testing (Linux, macOS, Windows)
- [ ] GoReleaser config for binary distribution

**Deliverable**: Ready for AGENTS.md deployment.

---

## AGENTS.md Blurb

Add this section to AGENTS.md:

```markdown
## slb — Simultaneous Launch Button (Dangerous Command Authorization)

**slb** implements a two-person rule for destructive commands. Before running commands that match dangerous patterns, you MUST get approval from another agent.

### Why This Exists

When multiple agents work in parallel, one agent's mistake can destroy another's work or critical infrastructure. A second opinion catches errors before they become irreversible.

### Forgotten How to Use slb?

Just run `slb` with no arguments - it prints a quickstart guide.

### Quick Start

```bash
# 1. Start your session (do this once when you begin)
SESSION=$(slb session start --agent "<YourAgentName>" --program "claude-code" --model "opus-4.5" --json)
SESSION_ID=$(echo "$SESSION" | jq -r '.session_id')

# 2. Check if a command needs approval
slb patterns test "rm -rf ./build" --json
# {"needs_approval": true, "tier": "dangerous", "min_approvals": 1}

# 3. Request approval for dangerous commands
REQUEST=$(slb request "rm -rf ./build" \
  --session-id "$SESSION_ID" \
  --reason "Removing stale build artifacts" \
  --expected-effect "Deletes ./build directory" \
  --goal "Free up disk space before rebuild" \
  --safety "Build dir is regenerated, no source code affected" \
  --json)
REQUEST_ID=$(echo "$REQUEST" | jq -r '.request_id')

# 4. Wait for approval (blocks until approved/rejected/timeout)
slb status "$REQUEST_ID" --wait --json

# 5. Execute when approved
slb execute "$REQUEST_ID" --json

# 6. End session when done
slb session end --session-id "$SESSION_ID"
```

### As a Reviewer

Check for pending requests periodically:
```bash
slb pending --json
slb review <request-id> --json
slb approve <request-id> --session-id "$SESSION_ID" --reason-response "Verified safe"
# OR
slb reject <request-id> --session-id "$SESSION_ID" --reason "Path contains source code"
```

Or run in watch mode (will prompt for each pending request):
```bash
slb watch --session-id "$SESSION_ID"
```

### Risk Tiers

| Tier | Requires | Examples |
|------|----------|----------|
| CRITICAL | 2+ approvals | `DROP DATABASE`, `terraform destroy`, `kubectl delete node` |
| DANGEROUS | 1 approval | `rm -rf`, `git reset --hard`, `kubectl delete deployment` |
| CAUTION | Auto-approved (30s) | `rm *.log`, `git stash drop` |

### All Commands Support --json

Every slb command outputs structured JSON with `--json`. Parse with jq:
```bash
slb pending --json | jq '.[] | select(.risk_tier == "critical")'
```

### What to Include in Requests

1. **Reason**: Why do you need to run this command?
2. **Expected Effect**: What will actually happen?
3. **Goal**: What are you trying to achieve?
4. **Safety Argument**: Why is this safe or reversible?

Be specific. "Cleaning up" is not enough. "Removing ./build directory (500MB of compiled artifacts) to fix out-of-space error before next build" is good.

### What to Check When Reviewing

1. Does the reason make sense?
2. Is the expected effect accurate?
3. Does this align with AGENTS.md rules?
4. Is there a safer alternative?
5. Has the dry-run output been reviewed?

When in doubt, reject and ask for clarification.

### Adding New Dangerous Patterns

If you encounter a command that SHOULD require approval but doesn't, ADD IT:

```bash
slb patterns add --tier dangerous "^helm upgrade.*--force" \
  --reason "Force upgrades can cause downtime"
```

You can freely ADD patterns (making things safer). You CANNOT remove patterns - that requires human approval via `slb patterns request-removal`.

### Never Bypass slb

Do NOT run dangerous commands directly. Even if you're confident. The point is peer review, not just approval.

Human operators can use `slb tui` for a visual dashboard, or `slb emergency-execute` for urgent overrides with full logging.
```

---

## Future Enhancements

### v1.1: Learning Mode

- Track which commands get approved vs rejected
- Track which executed commands caused subsequent problems
- Generate pattern recommendations based on history
- Anomaly detection: "This agent has unusually high rejection rate"

### v1.2: Team Features

- Named reviewer groups ("infra-team", "senior-devs")
- Escalation chains: Agent → Senior Agent → Human
- Scheduled approval windows (no CRITICAL approvals after 6pm)

### v1.3: Cloud Sync

- Optional cloud backup of history
- Cross-machine session management
- Team dashboard (web UI)

### v1.4: ML-Assisted Review

- Suggest approval/rejection based on historical patterns
- Highlight unusual aspects of requests
- Auto-generate review responses

---

## Open Questions

1. **Single vs multiple binaries**: Should daemon be separate binary or `slb daemon start` spawns subprocess?

   *Recommendation*: Single binary, daemon runs as subprocess for simplicity.

2. **Windows support priority**: How important is Windows support initially?

   *Recommendation*: Linux/macOS first, Windows later (file watching differs significantly).

3. **Multi-project awareness**: Should a single daemon handle multiple projects?

   *Recommendation*: Yes, one user-level daemon monitoring all projects with .slb/ directories.

4. **Rate limiting**: Should there be limits on request frequency?

   *Recommendation*: Track but don't limit initially. Alert on anomalies.

---

## Appendix: Pattern Matching Details

### Pattern Syntax

Patterns use regex with these conventions:
- Case-insensitive by default
- `^` anchors to command start
- `\s+` for whitespace
- `(?!...)` for negative lookahead
- `.*` for any characters

### Pattern Precedence

When a command matches multiple patterns:
1. Check SAFE patterns first → skip entirely
2. Check CRITICAL → highest risk wins
3. Check DANGEROUS
4. Check CAUTION
5. No match → allowed without review

### Path-Aware Patterns

Some patterns should consider paths:
```toml
# More dangerous if path is outside project
[patterns.critical.context]
pattern = "^rm\\s+-rf"
require_path_check = true
dangerous_paths = ["/", "~", "/etc", "/var", "/usr"]
```

---

## Appendix: Example Request JSON

```json
{
  "id": "req-a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "projectPath": "/data/projects/myapp",
  "command": "kubectl delete node worker-3",
  "riskTier": "critical",
  "requestor": {
    "sessionId": "sess-1234",
    "agentName": "BlueDog",
    "model": "gpt-5.1-codex"
  },
  "justification": {
    "reason": "Worker-3 has been in NotReady state for 15 minutes after kernel panic",
    "expectedEffect": "Node removed from cluster, pods already evicted",
    "goal": "Clean up cluster state by removing dead node reference",
    "safetyArgument": "Node is dead, removal is cosmetic cleanup, can re-provision later"
  },
  "dryRun": {
    "command": "kubectl delete node worker-3 --dry-run=client",
    "output": "node \"worker-3\" deleted (dry run)"
  },
  "attachments": [
    {
      "type": "file_snippet",
      "content": "NAME       STATUS     ROLES    AGE\nworker-1   Ready      <none>   5d\nworker-2   Ready      <none>   5d\nworker-3   NotReady   <none>   5d"
    }
  ],
  "status": "pending",
  "minApprovals": 2,
  "requireDifferentModel": false,
  "createdAt": "2025-12-13T14:32:05Z",
  "expiresAt": "2025-12-13T15:02:05Z"
}
```

---

## Installation & Distribution

### One-Line Install

```bash
curl -fsSL https://raw.githubusercontent.com/Dicklesworthstone/slb/main/install.sh | bash
```

### Go Install

```bash
go install github.com/Dicklesworthstone/slb/cmd/slb@latest
```

### Homebrew (macOS/Linux)

```bash
brew install dicklesworthstone/tap/slb
```

### Shell Completions

After installing, add to your shell rc file:

```bash
# zsh (~/.zshrc)
eval "$(slb completion zsh)"

# bash (~/.bashrc)
eval "$(slb completion bash)"

# fish (~/.config/fish/config.fish)
slb completion fish | source
```

Shell completions provide:
- Tab completions for all commands and flags
- Request ID completion from pending list
- Session ID completion from active sessions

---

## NTM Integration

slb integrates naturally with NTM for multi-agent orchestration:

```bash
# In your NTM session, agents use slb for dangerous commands
ntm send myproject --cc "Use slb to request approval before any rm -rf or kubectl delete commands"

# slb watch can run in a dedicated pane
ntm add myproject --cc=1  # Dedicated reviewer agent
ntm send myproject:cc_added_1 "Run 'slb watch' and review all pending requests carefully"
```

**Command palette integration**: Add to your NTM config.toml:

```toml
[[palette]]
key = "slb_pending"
label = "SLB: Review Pending"
category = "Safety"
prompt = "Check slb pending and review any dangerous command requests"

[[palette]]
key = "slb_status"
label = "SLB: Check Status"
category = "Safety"
prompt = "Run 'slb pending --json' to see all pending approvals and 'slb sessions --json' to see active agents"
```

---

## Appendix: Quick Reference Card

This is what `slb` (no args) prints. Copy the text version to AGENTS.md if needed:

```
┌─────────────────────────────────────────────────────────────────────────┐
│  ⚡ SLB QUICK REFERENCE - Dangerous Command Approval                    │
├─────────────────────────────────────────────────────────────────────────┤
│  START SESSION (once):                                                  │
│    slb session start -a MyName -p claude-code -m opus-4.5 -j            │
│                                                                         │
│  BEFORE DANGEROUS COMMAND:                                              │
│    slb check "rm -rf ./build"              # Is approval needed?        │
│    slb request "rm -rf ./build" -s $SID \                               │
│      --reason "..." --expected-effect "..." --goal "..." --safety "..." │
│    slb status $REQ_ID --wait -j            # Wait for approval          │
│    slb execute $REQ_ID -j                  # Run when approved          │
│                                                                         │
│  AS REVIEWER:                                                           │
│    slb pending -j                          # Check for pending          │
│    slb review $ID -j                       # Read details               │
│    slb approve $ID -s $SID                 # Approve                    │
│    slb reject $ID -s $SID --reason "..."   # Reject                     │
│                                                                         │
│  ADD DANGEROUS PATTERNS (you can do this!):                             │
│    slb patterns add --tier critical "^pattern" --reason "..."           │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│  TIERS: 🔴 CRITICAL (2)  🟠 DANGEROUS (1)  🟡 CAUTION (auto-30s)        │
│  FLAGS: -s/--session-id  -j/--json   FORGOT? Just run: slb              │
└─────────────────────────────────────────────────────────────────────────┘
```

---

*Document version: 1.4*
*Created: 2025-12-13*
*Updated: 2025-12-13*
*Changes:*
- *v1.0: Initial comprehensive plan*
- *v1.1: Updated to Go + Charmbracelet stack (matching NTM)*
- *v1.2: Removed robot mode (CLI is agent-first by design), base `slb` shows quickstart, added huh/lo/go-pretty/conc libraries*
- *v1.3: Fixed DELETE without WHERE (now CRITICAL), fixed section numbering, replaced TypeScript with Go in examples, simplified Edge Case 1 (no self-approval), added `slb check` alias, added `-s/-j/-p` short flags, added `slb version`, added tests to implementation phases, fixed `slb completion` (was `slb init`)*
- *v1.4: Added pattern management (agents can ADD but not REMOVE patterns), `slb patterns add/remove/request-removal/suggest`, pattern_changes and custom_patterns tables, base `slb` now shows colorful quick reference card with lipgloss styling*
*Status: Ready for review*
