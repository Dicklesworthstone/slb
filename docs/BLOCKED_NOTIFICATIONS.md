# Blocked-command alerts

SLB can notify peer reviewers when its command hooks block dangerous or critical
commands, including repeated attempts that need attention. This is an opt-in
extension of the blocked audit, separate from pending-request notifications.
It does not approve, execute, reject, or create requests.

## Enable for a project

Edit the project's `.slb/config.toml` (merge these settings with existing sections,
do not duplicate TOML table headers), then restart that project's SLB daemon.
For Agent Mail delivery:

```toml
[notifications]
desktop_enabled = false

[notifications.blocked]
enabled = true
window_seconds = 600
repeat_threshold = 3
cooldown_seconds = 60
max_per_minute = 10
agent_mail_url = "http://127.0.0.1:8765/mcp/"
agent_mail_sender = "BlueLake"
agent_mail_recipients = ["GreenCastle", "RedStone"]
# Optional: an existing inbox designated by the operator for repeated alerts.
# human_recipient = "GoldenOwl"

[integrations]
agent_mail_enabled = true
agent_mail_thread = "SLB-Reviews"
```

Use your **existing registered Agent Mail identities**, not the example names.
The sender must have permission to contact every recipient in the same Agent
Mail project. The project key is the daemon's canonical absolute project path.
SLB does not register identities, change contact policies, impersonate a human,
or assume that `SLB-Broadcast` or a special human account exists.

When your Agent Mail server requires credentials, set them in the environment
that launches the daemon:

```sh
export SLB_AGENT_MAIL_TOKEN='<HTTP bearer token>'
export SLB_AGENT_MAIL_SENDER_TOKEN='<token for the configured sender identity>'
slb daemon start
```

The first token authenticates HTTP transport; the second is passed as
`sender_token` to `send_message`. They are separate credentials. Omit only those
not required by your server. They are not TOML fields and are not placed in
alert bodies or delivery ledgers. The server's authentication and contact
requirements still apply.

This new blocked-alert route uses Agent Mail's HTTP MCP endpoint directly. It
initializes a session, negotiates a supported protocol version, calls
`send_message`, and requires positive delivery confirmation. JSON and SSE tool
responses are supported. A missing service, error response, empty delivery,
malformed response, or timeout is a delivery failure, not a success. Existing
request-lifecycle notifications still use their existing adapter; this does
not replace that separate integration.

To disable blocked alerts, set `notifications.blocked.enabled = false` and
restart the daemon. Disabled alerts do not read templates or audit records,
create delivery files, or contact destinations. The nested blocked settings
are read from TOML; they do not currently have `slb config set/get` key bindings
or individual `SLB_*` environment overrides.

## Other destinations

To use desktop alerts, enable the existing `notifications.desktop_enabled`
setting along with `notifications.blocked.enabled`. Linux uses `notify-send`;
macOS uses `osascript`. Headless/unavailable desktop services are failures that
are retried under the same budget. Windows desktop delivery is not implemented.

To use a JSON webhook, configure the existing `notifications.webhook_url`:

```toml
[notifications]
desktop_enabled = false
webhook_url = "https://alerts.example.invalid/slb"

[notifications.blocked]
enabled = true
```

The webhook receives a JSON object containing `event` (`blocked_command` or
`repeated_blocked_command`) and `alert`. The alert includes the redacted command,
command fingerprint, project, working directory, session label, tier, attempt
count, and first/last timestamps. It is a generic JSON webhook, not a
Slack/Discord-specific payload. Only a 2xx response counts as delivery.
`X-SLB-Alert-ID` supplies a stable identifier for destination-side deduplication
of the same grouped alert update.

Agent Mail and webhook URLs require HTTPS except for loopback HTTP. URL
userinfo and fragments are rejected, and redirects are never followed.
Remote response bodies and credential-bearing URLs are excluded from transport
error messages. Agent Mail, webhook, and desktop delivery may be combined;
each route has independent success/retry state.

## Escalation and delivery limits

Only `action=block` audit records with tier `dangerous` or `critical` qualify.
Confirmation prompts (`ask`), allowed commands, unknown tiers and caution-tier
blocks do not trigger these alerts. A blocked event is not proof that a command
executed, and its session ID is an audit label, not authenticated agent identity.

Attempts are grouped by project, exact working directory, session label and
command fingerprint. Separate commands or sessions cannot inflate each other's
counts. Nested initialized projects are excluded from their parent's scan;
traffic in unrelated projects cannot displace relevant records through a
shared newest-N cutoff. Repeated event IDs are counted once.

With default settings, three attempts within ten minutes make an alert
repeated. A first dangerous block has normal Agent Mail importance; critical
or repeated blocks are urgent. Repeated alerts also request acknowledgment and
copy `human_recipient` when configured. Repetition alone does not establish
malicious intent: reviewers should inspect the actual context.

An unchanged alert is suppressed after successful delivery. New attempts can
produce reminders after the cooldown. Crossing the repeat threshold or becoming
critical may produce an escalation before that cooldown, but never bypasses
the shared delivery-attempt budget. The default budget is ten attempts per
minute across all enabled routes, including failed sends. A failed route is
retried after 15, 30, 60, 120, then at most 240 seconds between attempts; a
successful route is not resent just because another route failed.

Zero or omitted numeric values select the defaults above. Window/cooldown
settings support 1..86400 seconds, repeat thresholds 2..10000, and delivery
budgets 1..1000 per minute. Invalid alert settings are reported in daemon logs;
they do not weaken or disable the approval notary.

## Templates

Optional `blocked_template` and `repeated_template` paths in the blocked section
select operator-authored Go text templates for Agent Mail message bodies.
`~/` expands to the daemon user's home directory. Templates have access only to
`Alert` fields and standard text-template operations, with no environment,
filesystem, command execution or network functions. For example:

```text
Blocked {{.Tier}} command in {{.Project}}
Session label: {{.SessionID}}
Attempts: {{.Attempts}} within {{.WindowSeconds}} seconds
Command: {{printf "%q" .CommandRedacted}}
Inspect slb audit before requesting independent review.
```

Templates are limited to 16 KiB and rendered bodies to 32 KiB. Invalid templates
or oversized output are errors, not silent truncation. Subjects intentionally
omit command text. JSON webhooks keep their structured format; desktop messages
use a short fixed summary rather than putting command text in a script.

## Audit, recovery and operational boundaries

The daemon checks immediately on startup and then every five seconds in a
separate worker. Hook decisions never wait for network notification delivery.
Each scan/delivery pass has a 15-second context budget, and each transport attempt
has at most ten seconds. Shutdown cancels and joins the worker.

The source is `~/.slb/audit/blocked`: records from the daemon hook query path,
native Claude command guard and generated offline Python guard share this
schema. Other paths are included only when they write that audit schema; native
Git authorization logs are not implicitly converted into blocked-command alerts.
Recent denials made while the daemon was down are picked up on restart. Events
older than the configured window remain in audit history but are not delivered
as stale alerts.

Delivery suppression and retry budgets are stored in the private file
`.slb/notifications/blocked-delivery.json`. Attempt reservations are synced before
transport I/O; success is recorded only after the destination reports success.
Corrupt, non-private, symlink or oversized ledgers stop delivery and produce a
diagnostic rather than resetting state and causing a notification flood.
Delivery ledgers contain hashed correlation/destination keys and timestamps,
not message bodies, endpoint URLs or credentials. Audit records remain the
source of blocked-command details; `slb audit` provides inspection and retention.

Run **one daemon per project ledger**. The dispatcher serializes calls within
one process, not independent processes. This is not exactly-once delivery: a
crash after an external send but before its receipt is saved, or an ambiguous
remote response, can cause a retry duplicate. Destination-side deduplication
can help for webhooks. Delivery also is not guaranteed after the event ages out
of the window. Alert failure never grants execution permission.

Aggregation supports up to 10,000 relevant events per window and delivery state
up to 4 MiB. Overflow or corrupt published audit records is reported instead of
presenting incomplete counts as complete. Shorten the window or inspect/retain
the audit explicitly when these limits are reached; the worker never prunes
source audit records. Redaction quality depends on the hook that recorded the
event: the worker uses `command_redacted` and never retrieves raw command argv.

Configuration and templates are loaded when the daemon starts; restart after
changes. Use daemon logs for delivery failures and recovery. No `slb notify test`
or `slb notify history` commands are introduced by this implementation.
