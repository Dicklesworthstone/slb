# Durable request lifecycle notifications

SLB can notify reviewers about committed request, review, status and execution
changes through Agent Mail HTTP MCP or JSON webhooks. This opt-in service reads
the existing transactional request journal rather than relying on a synchronous
CLI callback or a live subscription that can miss events during downtime.
It never approves or executes a request.

## Enable

Merge this section into the project's `.slb/config.toml`, using existing,
registered Agent Mail identities that can contact one another:

```toml
[notifications.requests]
enabled = true
agent_mail_url = "http://127.0.0.1:8765/mcp/"
agent_mail_sender = "BlueLake"
agent_mail_recipients = ["GreenCastle", "RedStone"]
lookback_seconds = 86400
max_per_minute = 60
```

The daemon's absolute project path is the Agent Mail project key. The service
uses the `SLB-Requests` thread and does not create identities, change contact
policies, invent a broadcast alias or act as a human. Where required, provide
`SLB_AGENT_MAIL_TOKEN` (HTTP bearer token) and `SLB_AGENT_MAIL_SENDER_TOKEN`
(sender identity token) in the daemon environment. Restart the project daemon
after changing settings or templates.

For webhooks, add `webhook_url` inside `[notifications.requests]`. It may be
used alone or alongside Agent Mail. This separate endpoint avoids sending new
lifecycle traffic to a preexisting blocked-alert destination without an opt-in.
HTTPS is required except for loopback HTTP; URL userinfo/fragments and redirects
are rejected. Webhooks receive the notice directly as JSON and a stable
`X-SLB-Event-ID` header suitable for destination-side deduplication.

Request-journal delivery has its own explicit enable/destination settings.
`integrations.agent_mail_enabled` still controls legacy callback construction
and the blocked-alert mail route, not this worker. When the normal project/user
configuration enables request-journal delivery, newly constructed legacy request
clients suppress synchronous CLI sends: the journal worker handles the committed
mutation instead. Existing clients hold their configuration until recreated.
An explicit CLI `--config` file does not replace the normal daemon configuration;
configure this feature in the shared project/user TOML, then restart the daemon.

The previous pending desktop/webhook notifier and the blocked-alert worker remain
separate. Their destinations can overlap, producing intentionally different
notices. Disable an old route separately when its additional reminders are not
wanted. A project with request notifications disabled retains legacy behavior.

## What reviewers receive

Notices preserve the journal event's metadata at the time of the mutation:
request ID, project, event kind, occurrence time, status, risk tier, command hash,
requester session ID, required approvals/model diversity, submitted review counts,
review decision when present, and recorded exit code when present. The worker
covers creation, baseline, updates, status changes, review additions/changes/removal,
and request deletion/movement events. It does not synthesize older history.

**No command text, argv, display text, justification, attachments, review comments,
preview output, log paths, session keys or execution receipts are sent.** Reviewers
must use `slb review <request-id>` in the named project for current state and full
evidence. Journal cursors are private delivery state and are not transmitted,
even by a custom message template. Subjects are fixed text, not raw commands.

Critical pending requests and escalations are urgent; operator-attention notices
request acknowledgment. An approval notice says to recheck permission before
execution. Submitted review-row counts are not proof of valid signed quorum.
A replayed notice can describe an older state of an already completed or changed
request. In particular, completion of a native Git authorization token records
hook permission, not proof that the eventual Git operation succeeded.

## Recovery and delivery semantics

Each destination has its own persisted cursor in
`.slb/notifications/request-delivery.json`. The cursor is advanced only after
successful external delivery, or an explicitly excluded initial historical event.
The source validates the journal identity and resume anchor on every read.
A replaced database, changed cursor anchor, corrupt ledger or malformed source
page stops that route with a diagnostic rather than resetting to the newest event.

On the first addition of a destination, `lookback_seconds` selects its historical
catch-up cutoff (default one day; supported 1 second through 30 days; zero selects
the default). Older events are scanned and skipped. This cutoff is then fixed:
a queued event does **not** expire because its destination stays offline. Events
committed while the daemon is down are delivered after restart from its cursor.
If no ledger has ever been created, the first-addition lookback still applies.
This is lifecycle replay, not a freshly synthesized snapshot of all pending work.

Before transport I/O, the service syncs its attempt reservation and retry deadline.
Success is acknowledged only after Agent Mail confirms a send or the webhook
returns 2xx. Failed/ambiguous sends retain the event and retry after 15, 30, 60,
120, then at most 240 seconds between attempts. A successful destination continues
independently while another is down. The shared budget defaults to 60 transport
attempts per minute, including failures (supported 1..1000; zero selects default).
Destination order rotates so the first configured route cannot monopolize every
pass. The budget and cursors survive restarts.

At most 64 journal events per destination are examined per pass; pagination
continues on subsequent passes without skipping to a new head. Large historical
journals can therefore take multiple passes to catch up. The worker checks on
startup and every five seconds, with a 15-second pass budget and ten-second
transport deadlines. Hook queries, request admission, reviews and execution do
not wait for it. Shutdown cancels and joins the worker before closing its DB.

Run one daemon per project ledger. Calls on one dispatcher are serialized, not
independent processes. A crash after a peer receives a message but before the
cursor is saved can produce a duplicate on retry. This is **not exactly-once**
delivery. Event IDs remain stable for retries. Destination changes (including
recipient changes) get a new checkpoint and initial lookback; token rotation does
not reset checkpoints. Up to 50 active routes and 100 retained destination
checkpoints are supported; ledger size is capped at 4 MiB. Retained inactive
checkpoints are not silently discarded.

## Templates, configuration failures and diagnostics

An optional `template` path in the request section selects a Go text template.
It can access `RequestNotice` metadata only; `Cursor` is cleared before rendering.
There are no shell, environment, filesystem or network template functions.
Template input is limited to 16 KiB and output to 32 KiB. `~/` paths expand to the
daemon user's home directory. Webhooks retain their structured JSON format.

Invalid route settings/templates are diagnosed without disabling the approval
notary. Invalid JSON/privacy/symlink/size state checks stop delivery instead of
resetting it and flooding peers. Delivery errors never grant execution rights.
Read daemon logs for delivery failures and retain the ledger when investigating
an outage or database restore; it is the evidence of what was acknowledged.
A backwards wall-clock jump pauses delivery until the persisted time is reached,
so it cannot reset the rate budget.

These nested settings are read through TOML and do not yet have individual
`slb config get/set` key bindings or dedicated `SLB_*` environment overrides.
No new notify CLI commands are introduced here. The Go integration tests cover
the actual journal/daemon/TOML paths; run the full repository tests with its
required toolchain before deployment.
