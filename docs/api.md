# ocis-agent REST API

The sidecar serves its REST API under `/agent-api/`. The reverse proxy in front of oCIS routes
`https://<oCIS host>/agent-api/` to the sidecar, so the extension calls it on the same origin as
oCIS Web. The sidecar never serves `/agent/`: those are the extension's UI routes and belong to
oCIS Web.

- Base path: `/agent-api/v1`, plus the unauthenticated `GET /agent-api/healthz`.
- Bodies are JSON (`Content-Type: application/json`, only a `charset=utf-8` parameter is allowed).
- Times are RFC 3339 strings in UTC; a time that does not apply is `null`.
- Ids (jobs, approvals, conversations, messages) are 26-character ULIDs in upper case. A path id of
  any other shape is `404 not_found`, a query id of any other shape `400 invalid_request`.

## Authentication and request rules

Every `/agent-api/v1` request passes these steps, in this order:

1. **Recover, request id, security headers.** Every response carries `X-Request-Id` (16 hex
   digits; a client-supplied one is ignored), `X-Content-Type-Options: nosniff`,
   `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`,
   `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` and
   `Cross-Origin-Resource-Policy: same-origin`. A panic is `500 internal`. Unmatched paths are a
   JSON `404 not_found`, a wrong method a JSON `405 method_not_allowed` with `Allow`. Paths that
   are not clean (`//`, `.` or `..` segments) are `404`.
2. **Failed-login bucket per client IP.** The key is the TCP peer address (IPv6 by its /64);
   `X-Forwarded-For` is never trusted, so behind a reverse proxy all users share the proxy's
   bucket. A bearer the sidecar has neither cached (about 60 s) nor seen accepted recently (up to
   12 h) reserves one token of the bucket before it is sent to oCIS. The reservation is refunded
   on success and on failures that are not the client's (oCIS outage, `422`, an Origin refusal); it
   stays charged for a missing, malformed or rejected bearer and when the client hangs up. The
   bucket has 10 times the per-user rate and burst. An empty bucket is `429 rate_limited` with
   `Retry-After`; cached and known tokens still pass.
3. **Origin check** for every method other than `GET` and `HEAD`: exactly one `Origin` header,
   byte-equal to `AGENT_ALLOWED_ORIGIN` (default: the oCIS URL, lower-case host, no default port),
   and `Sec-Fetch-Site`, if sent, must be `same-origin`. Otherwise `403 origin_rejected`, before the
   token is looked at.
4. **Bearer token.** Exactly one `Authorization: Bearer <token>` header (a token68 of at most
   8 KiB), the user's oCIS OIDC access token. oCIS itself decides: the sidecar asks Graph `/me`
   and the personal drive with it. Basic auth (an app token) is never accepted. Missing, malformed
   or rejected: `401 unauthenticated` with `WWW-Authenticate: Bearer`. oCIS unreachable:
   `502 upstream_unavailable`; no answer from oCIS before the deadline: `503 unavailable`; an
   account without a login name: `422 no_login_name`. The
   identity (user id, login name, drive) comes only from this validation, never from headers,
   the query or the body, and every lookup is scoped by it: another user's ids are `404`.
5. **Per-user rate limit**: `AGENT_API_RATE` requests per second with a burst of
   `AGENT_API_BURST` (defaults 20 and 40); over it `429 rate_limited` with `Retry-After`.
6. **Visit.** The user's last visit is recorded at most once a minute (it clears a `dormant` pause).
7. **Body rules.** `POST` and `PUT` with a body need `Content-Type: application/json`
   (`415 unsupported_media_type`); other methods must not send a body (`400`). Body limits:
   64 KiB by default, `PUT /settings` 128 KiB, the memory routes 200 KiB, chat messages 32 KiB
   (`413 too_large`). A body is one JSON object of at most 2048 values, valid UTF-8 without
   unpaired surrogate escapes, keys exactly as documented (case variants, duplicates and unknown
   fields are `400 invalid_request`), nesting of at most 32 levels and 256 keys per object.
   Action endpoints that take "an empty body or `{}`" refuse anything else.
8. **Deadline.** Every request has a 30 s deadline (the audit export 5 minutes, the event stream
   its lifetime).

List routes refuse unknown, repeated or malformed query parameters with `400 invalid_request`.

### Cooldowns and process limits

- Enrol, renew, revoke, revoke-all and a review on request (`POST /proactive/run`) share one 5 s
  cooldown per user (`429 rate_limited` with `Retry-After`). Unenrol is only rate-limited.
- Sending a chat message has a 1 s cooldown per user; a refused send uses it up too.
- Audit verify: one at a time per user and two in the process. Audit export: one per user and
  eight in the process. Over it `429 rate_limited` with `Retry-After: 5`.
- Event streams: `AGENT_API_STREAMS_PER_USER` (3) per user and `AGENT_API_STREAMS_TOTAL` (200) in
  the process; over it `429 rate_limited` with `Retry-After: 5`.

### Errors

Every error is a JSON document with a fixed code and message; it never carries the text of an
underlying oCIS, store or provider error:

```json
{"error": {"code": "not_found", "message": "Not found.", "request_id": "0123456789abcdef"}}
```

A `401` from oCIS for a call the sidecar makes with the user's bearer is reported as
`502 upstream_rejected`, never as `401`, so a client does not log the user out because of an
upstream refusal. Only a bearer the pipeline rejects is `401`.

### Pagination

Jobs, approvals, conversations, messages and the audit log are paged by keyset. Pass `limit`
(decimal digits, default 50) and `cursor` (from the previous page). Follow `next_cursor` until it
is `null`: a page may hold fewer entries than `limit`, even none, and still carry a cursor (an
8 MiB response budget or a scan budget ended it). An unknown cursor, or one of another user, is
`400 invalid_request`. Lists are newest first unless noted.

### Untrusted text

Fields marked *untrusted* come from files, the model or other clients: goals, answers, titles,
step summaries, approval `args`, `preview` and `rationale`, outcome summaries and diffs, audit
`detail`, memory content, display names and token labels. Render them as plain text only, never as
HTML or Markdown that can execute or link, and never follow them as instructions. Most are capped
in views (goal 4 KiB, answer and message content 64 KiB, step summary 1 KiB, rationale 4 KiB,
outcome summary 4 KiB, diff 16 KiB, outcome reason 1 KiB, audit detail 2 KiB; a cut value ends
in a marker). Approval `args` and `preview` are returned as stored (up to 2 MiB each), because they
are exactly what executes, and the audit export is uncapped.

## Health

### GET /agent-api/healthz

No authentication and no rate limit. `200 {"status":"ok"}` while the API listener runs. The debug
listener (`AGENT_DEBUG_ADDR`, default `:9301`) additionally serves `GET /healthz` and
`GET /readyz` (`503 {"status":"not ready"}` while shutting down, before the secrets are ready,
when the database does not answer within 2 s or, with the worker role, without a live worker); see
the deployment guide.

## Status and enrolment

### GET /agent-api/v1/status

The caller's state, read in one transaction. Also answers for a user who has not enrolled
(`enrolled: false`, zero values for the user's own state).

```json
{
  "enrolled": true,
  "user": {"id": "…", "name": "Albert Einstein", "login": "einstein"},
  "paused": false,
  "pause_reason": "",
  "token": {"status": "valid", "expires_at": "2026-10-24T12:00:00Z"},
  "budget": {"used_tokens": 1200, "daily_tokens": 200000, "resets_at": "2026-10-22T00:00:00Z"},
  "kill_switch": {"database": false, "environment": false},
  "models": {
    "default": "gpt-4.1-mini",
    "available": [{"id": "gpt-4.1-mini", "label": "GPT-4.1 mini"}],
    "preferences": {"chat": null, "proactive": null}
  },
  "pending_approvals": 0
}
```

- `user.name` is the display name (untrusted), `user.login` the login name.
- `pause_reason`: `""`, `user`, `dormant`, `token_anomaly`, `credential_invalid`,
  `credential_revoked` or `credential_expired`.
- `token.status`: `valid`, `invalid`, `revoked` or `none`; `expires_at` is null when revoked or
  none.
- `budget`: tokens used today (UTC) and the daily budget (the user's setting, never above the
  operator's `AGENT_USER_DAILY_TOKENS`; for a user not enrolled, the budget an enrolment starts
  with). The day resets at UTC midnight.
- `kill_switch.database` is the flag set by `ocis-agent killswitch`, `environment` is
  `AGENT_KILL_SWITCH`.
- `models`: the operator's catalog (empty without a model configured) and the user's
  preferences per task kind, shown only while they are in the catalog.
- `pending_approvals`: the exact number of pending approvals of jobs that have not ended.

### POST /agent-api/v1/enroll

Empty body or `{}`. Creates the agent's app token with the caller's bearer (`201` for a new
enrolment, `200` for a re-enrolment, which replaces the token), body: the status. Cooldown: 5 s.
Errors: `422 no_login_name`, `502 upstream_rejected` / `upstream_unavailable` (oCIS refused or
failed the token creation), `429 rate_limited`.

### POST /agent-api/v1/unenroll

Body empty or `{"delete_memory": true|false}` (default false). Deletes every app token of this
instance, optionally the memory folder `/.ocis-agent` (with the caller's bearer), audits
`unenrolled` and deletes ALL of the user's state in the sidecar: policy, app token and token
labels, jobs and their steps, approvals, idempotency records, cursors, and every conversation with
its messages (the whole chat history). Only the audit log is kept. Memory files in oCIS stay
unless `delete_memory` is true. `204`. `404 not_found` when not enrolled. A failure before the last step leaves the user in
place, so the call can be repeated.

### DELETE /agent-api/v1/token

Deletes the agent's own app tokens and pauses the agent with `credential_revoked`. No body.
`200 {"revoked": <number of deleted tokens>}`. Cooldown: 5 s. `502 upstream_unavailable` if oCIS
still lists a token after the delete (nothing changed locally; retry).

### GET /agent-api/v1/token/revoke-all/preview

All of the caller's app tokens, for the confirmation of the incident revoke. Never a hash or a
value.

```json
{"tokens": [{"label": "ocis-agent-…", "created_at": "…", "expires_at": "…", "ours": true}]}
```

`label` is set by whatever client created the token (untrusted); `ours` marks this instance's
tokens.

### POST /agent-api/v1/token/revoke-all

Body exactly `{"confirm": true}` (anything else is `400` and nothing is sent to oCIS). The incident
revoke: deletes ALL of the caller's app tokens, including those of other clients, and pauses the
agent with `credential_revoked`. `200 {"revoked": n}`. Cooldown: 5 s.

### POST /agent-api/v1/token/renew

Empty body or `{}`. Replaces the agent's app token now (the sidecar does this in the background
anyway). `200 {"renewed": true, "old_cleaned": true|false}`; `old_cleaned` is false when the old
token could not be deleted yet (the sweep deletes it later). `409 credentials_invalid`: the stored
token no longer works, enrol again. Cooldown: 5 s.

## Pause and resume

### POST /agent-api/v1/pause

Empty body or `{}`. The user's own pause: no job of the user is claimed until it is resumed.
A `credential_*` or `token_anomaly` pause already in place is kept (the request is audited as
`kept_pause`). `200` with the status. `404 not_found` when not enrolled.

### POST /agent-api/v1/resume

Empty body, `{}` or `{"reason": "user" | "token_anomaly"}` (default `user`). Clears only that
reason; a pause with another reason stays (no-op). Credential pauses are cleared by enrolling
again, `dormant` by any authenticated request. `200` with the status. Wakes the workers.

## Settings

### GET /agent-api/v1/settings

```json
{
  "proactive_enabled": false,
  "watch": ["/Projects"],
  "sweep_interval": "1h0m0s",
  "quiet_hours": "22:00-07:00",
  "daily_tokens": 200000,
  "daily_tokens_max": 200000,
  "dormancy_days": 30,
  "models": {"chat": null, "proactive": null}
}
```

`watch` holds the user's own paths (untrusted). `daily_tokens_max` (the operator's cap) and
`dormancy_days` are read-only. `404 not_found` when not enrolled.

### PUT /agent-api/v1/settings

A partial update: the fields present replace the stored ones, at least one must be present.

| Field | Rule |
| --- | --- |
| `proactive_enabled` | `true` or `false` |
| `watch` | array of at most 20 absolute paths of at most 1024 bytes, no `.`/`..` segments, no control or bidi characters, not inside `/.ocis-agent`; duplicates are dropped |
| `sweep_interval` | Go duration from `5m` to `168h`, whole seconds |
| `quiet_hours` | `""` or `HH:MM-HH:MM` in UTC (24 h, two different times; the start is quiet, the end is not; may wrap midnight) |
| `daily_tokens` | integer from 1000 to the operator's cap, or `null` to follow the operator's cap |
| `models` | `{"chat": id or null, "proactive": id or null}`; an id must be in the catalog, `null` clears the preference |

Anything else (unknown fields such as `auto_approve_write`, wrong types, out of range) is
`400 invalid_request`. A change is audited as `policy_changed` with the field names only.
`200` with the settings.

## Jobs

### GET /agent-api/v1/jobs

Query: `state` (`queued`, `running`, `waiting_approval`, `succeeded`, `failed`, `cancelled`),
`limit` (1-200), `cursor` (a job id). Newest first.

```json
{"jobs": [{
  "id": "…", "trigger_kind": "chat", "trigger_ref": "<conversation id>", "goal": "…",
  "state": "succeeded", "attempt": 0, "runs": 1,
  "created_at": "…", "updated_at": "…", "ended_at": "…",
  "error": "", "steps_count": 3, "tokens_in": 1500, "tokens_out": 200
}], "next_cursor": null}
```

- `trigger_kind` is `chat` for chat jobs, `schedule` for a scheduled review, `file_event` for a
  change in a watch folder and `manual` for a review on request; `trigger_ref` is the conversation
  id of a chat job and empty for every other job.
- `goal` (untrusted, capped) is the user's text of a chat job or a server-made instruction.
- `attempt` counts crashed runs, `runs` every claim.
- `ended_at` is when the job reached a terminal state (null while live).
- `error` is a fixed reason text of the runner, safe to show.
- `tokens_in` / `tokens_out` are the sums over the job's steps.

### GET /agent-api/v1/jobs/{id}

The job (fields as above) plus its timeline and final answer:

```json
{
  "id": "…", "state": "succeeded", "…": "…",
  "steps": [{"n": 2, "kind": "tool", "tool": "files.list", "call_id": "c1", "status": "done",
             "tokens_in": 0, "tokens_out": 0, "summary": "…", "approval_id": "", "created_at": "…"}],
  "answer": "…"
}
```

`kind` is `llm` (a model turn, with its token usage) or `tool` (a tool call, with `status` such as
`done`, `failed`, `unknown`, `rejected` or `denied`). `summary` (untrusted, capped) is tool-made text; `answer` (untrusted, capped) is the model's final
answer, empty while there is none.

### POST /agent-api/v1/jobs/{id}/cancel

Empty body or `{}`. Cancels a job that has not ended and audits `job_cancelled`. `200` with the job.
`409 conflict` if it has already ended. Its open approvals are settled to `expired` by the
scheduler; a running worker stops at its next write.

## Approvals

Every action other than a read or a memory write needs the user's approval. An approval shows
exactly what will execute; a decision is bound to the arguments hash the user saw.

Approval object:

```json
{
  "id": "…", "job_id": "…", "call_id": "c3", "tool": "files.write", "risk": "write",
  "args": {"path": "/Notes/todo.md", "content": "…"},
  "args_hash": "sha256:…", "target_etag": "",
  "rationale": "…", "preview": {"…": "…"},
  "state": "pending",
  "requested_at": "…", "expires_at": "…",
  "decided_by": "", "decided_at": null, "executed_at": null,
  "outcome": null,
  "job_ended": false,
  "needs_verification": false,
  "acknowledged_at": null, "acknowledged_by": ""
}
```

- `risk`: `read`, `write`, `destructive` or `outward`.
- `args` and `preview` (untrusted, uncapped) are the stored JSON, exactly what executes;
  `rationale` (untrusted, capped) is the model's unverified explanation.
- `target_etag`: the ETag of an existing target the action was prepared against.
- `state`: `pending`, `approved`, `rejected`, `expired`, `executing`, `executed`, `failed` or
  `unknown`.
- `decided_by`: the Graph user id of the deciding user.
- `outcome` (null while undecided or not yet run): `{"state", "summary", "reason", "diff"}` with
  `state` `done`, `failed`, `unknown`, `expired` or `rejected`; `summary` and `diff` are untrusted
  tool output (capped), `reason` is the executor's or a fixed text.
- `job_ended`: a pending approval whose job has already ended; it cannot be decided any more and
  is settled to `expired` soon.
- `needs_verification`: the outcome is uncertain (for example a crash during the call) and the
  user has not acknowledged it: check the target in oCIS, then acknowledge.

### GET /agent-api/v1/approvals

Query: `state` (default `pending`; `all` or one of the states above), `job_id`,
`needs_verification=true` (the only value; it keeps only approvals that need verification and
makes `state` default to `all`; one request examines at most 2000 candidates), `limit` (1-100),
`cursor` (an approval id). Newest first.
`{"approvals": [ … ], "next_cursor": null}`. `?state=pending` also lists pending approvals of ended
jobs (`job_ended: true`); `pending_approvals` in the status counts only those of live jobs.

### GET /agent-api/v1/approvals/{id}

One approval object.

### POST /agent-api/v1/approvals/{id}/decision

Body `{"decision": "approve" | "reject", "args_hash": "<the args_hash shown>"}`. `200` with the
updated approval; the job resumes once none of its approvals is pending, and the workers are woken.
Errors: `409 stale_arguments` (the hash is not the stored one: reload and show the approval again),
`409 already_decided`, `410 expired`, `409 job_ended`, `404 not_found`.

### POST /agent-api/v1/approvals/bulk-decision

Body `{"decision": "approve" | "reject", "items": [{"id": "…", "args_hash": "…"}]}`: 1 to 100
distinct pending approvals of ONE job, decided together (all or nothing). `200 {"approvals": [ … ]}`.
Items of different jobs or duplicate ids are `400 invalid_request`; the other errors are those of
a single decision (any stale hash is reported first).

### POST /agent-api/v1/approvals/{id}/acknowledge

Empty body or `{}`. The user has checked the uncertain outcome of the action; `needs_verification`
becomes false (the state stays, audited as `approval_acknowledged`). `200` with the approval, also
when it was acknowledged before. `409 not_acknowledgeable` when the outcome is not uncertain.

## Memory

The agent's memory files live in the user's personal space under `/.ocis-agent/memory/`:
`profile.md`, `projects.md` and `notes/<slug>.md` (`slug`: lower-case letters, digits and `-`,
1-64 characters, starting with a letter or digit). Any other name is `400 invalid_request`. These
routes use the caller's own bearer (never the agent's app token), work without enrolment, and
answer `503 unavailable` when the sidecar has no memory service. Content is untrusted. Editing
here never unlocks the model's right to replace a file: the model must read it again itself.

### GET /agent-api/v1/memory

`{"files": [{"name": "profile.md", "size": 120, "etag": "\"…\"", "modified": "…"}]}`, sorted by name;
empty when the folder does not exist.

### GET /agent-api/v1/memory/{name}

`profile.md`, `projects.md` (or an escaped `notes%2F<slug>.md`). Body
`{"name", "content", "etag", "truncated", "editable"}`, also the `ETag` header. A file larger than
64 KiB is returned cut with `truncated: true` and `editable: false` (saving it would destroy the
rest); a file larger than 2 MiB is `422 file_too_large`, a file that is not UTF-8 text
`422 not_text`, a missing one `404 not_found`. A file oCIS is still processing after an upload is
`503 unavailable` with `Retry-After`.

### GET /agent-api/v1/memory/notes/{slug}

The same for `notes/<slug>`, where `{slug}` is the file name `<slug>.md`.

### PUT /agent-api/v1/memory/{name}

Body `{"content": "…"}` (at most 64 KiB, else `413 too_large`) and the header
`If-Match: <etag as loaded>` (exactly one ETag, no `*` and no list). `200 {"name", "etag", "diff"}`
(`diff` untrusted, capped at 16 KiB). Errors: `428 precondition_required` without `If-Match`,
`400 invalid_request` for an `If-Match` that is `*`, a list, repeated or longer than 256 bytes,
`400 invalid_request` for content that is not text (control characters other than tab, newline
and carriage return), `412 precondition_failed` when the file changed since it was loaded,
`404 not_found` when it does not exist, `422 file_too_large` when the existing file is larger
than 2 MiB. The edit is audited as `memory_edited` with the name and the first 1500 bytes of the
diff; a write whose outcome is uncertain is not audited.

### PUT /agent-api/v1/memory/notes/{slug}

The same for `notes/<slug>`.

### POST /agent-api/v1/memory

Body `{"name": "notes/ideas.md", "content": "…"}`. Creates the file: `201 {"name", "etag", "diff"}`,
`409 already_exists` when it exists. Audited as `memory_edited`.

## Conversations and chat

A message the user sends becomes a chat job; the answer arrives as an `assistant` message (or a
`system` message with a fixed reason when the job fails). Chat jobs need a process with the worker
role; in an api-only deployment they stay queued.

Conversation object: `{"id", "title", "created_at", "updated_at"}` (`title` untrusted).

Message object:

```json
{"id": "…", "role": "user", "content": "…", "job_id": "…", "created_at": "…",
 "job": {"id": "…", "state": "succeeded", "error": ""}}
```

`role` is `user`, `assistant` or `system`; `content` is untrusted (capped at 64 KiB). `job` is the
current state of the job answering a user message (null for other roles): a job can end without
any answer (cancelled, or failed by the reaper), so show its state and error instead of waiting.

### POST /agent-api/v1/conversations

Empty body, `{}` or `{"title": "…"}` (at most 100 characters, no control characters). `201` with
the conversation. `409 not_enrolled`, `409 limit_reached` at 200 conversations.

### GET /agent-api/v1/conversations

Query `limit` (1-100), `cursor` (a conversation id). Most recently updated first (a conversation
that gets a message moves to the top: reload from the top on a change).
`{"conversations": [ … ], "next_cursor": null}`.

### GET /agent-api/v1/conversations/{id}

One conversation.

### PUT /agent-api/v1/conversations/{id}

Body `{"title": "…"}` (required; `""` clears it). `200` with the conversation.

### DELETE /agent-api/v1/conversations/{id}

Deletes the conversation and its messages, cancels its chat jobs that have not ended and audits
`conversation_deleted`, in one transaction. `204`. The jobs stay visible in `GET /jobs`.

### GET /agent-api/v1/conversations/{id}/messages

Query `limit` (1-100), `cursor` (a message id of this conversation). Newest first.
`{"messages": [ … ], "next_cursor": null}`.

### POST /agent-api/v1/conversations/{id}/messages

Body `{"content": "…"}`: 1 to 8192 bytes of UTF-8 without control characters other than tab,
newline and carriage return. Checked in one transaction, in this order:

1. enrolled, else `409 not_enrolled`;
2. the conversation is the caller's, else `404 not_found`;
3. fewer than 5 live chat jobs, else `429 too_many_pending` (`Retry-After: 5`);
4. fewer than 1000 chat jobs today (UTC), else `429 limit_reached` (`Retry-After` until UTC midnight);
5. today's token budget not used up, else `429 budget_exhausted` (`Retry-After` until UTC midnight).

Then the job and the message are stored, an untitled conversation is named after the first 60
characters, `chat_message` is audited and the workers are woken.

```json
{"message": { … },
 "job": {"id": "…", "state": "queued"},
 "agent": {"paused": false, "pause_reason": "", "kill_switch": false}}
```

`202`. The message is accepted while the agent is paused or the kill switch is on; `agent` tells
why nothing runs yet. Cooldown: 1 s per user (`429 rate_limited`).

## Proactive reviews

With `proactive_enabled`, the agent reviews the user's recent file changes and open tasks on its
own, every `sweep_interval`, outside the quiet hours (UTC). The first scheduled review comes one
interval after proactive work is switched on. A review only reads and proposes: every action
other than a read or a memory write becomes an approval. Its goal is a fixed text, never a file
name or other data. A review is not started while the kill switch is on or the agent is paused
(any reason), while another review (of any kind) is queued, running or waiting for approval,
after `AGENT_PROACTIVE_DAILY_JOBS` (24) reviews of the UTC day, or once 80% of the day's token
budget is used; a running review also stops at 80% (chat jobs may use the whole budget). A
skipped scheduled review is checked again a few minutes later (at the end of the quiet hours, or
at midnight UTC for the daily cap). A skip because of a live review, the daily cap or the budget
is recorded as `proactive_skipped` with the reason when the reason changes, and at most once a day
while it stays the same; pauses (already audited) and quiet hours are not recorded. A started
review is recorded as `proactive_started`.

The agent also checks the `watch` folders for changes (every `AGENT_WATCH_POLL_INTERVAL`, 5 min)
and starts a review (`trigger_kind` `file_event`) once they stayed unchanged for
`AGENT_WATCH_DEBOUNCE` (1 min) after a change, or at the latest 10 minutes after the first change
while they keep changing, under the same conditions as a scheduled one (a change that cannot
start a review stays pending, is checked again at the next poll and is recorded like a skipped
scheduled review). The first
check of a folder only records its state, switching proactive work on starts afresh, and
changing `watch` drops a pending change, so earlier changes never start a review. Changes inside `/.ocis-agent` (the agent's memory) do not
count, and neither do the agent's own approved writes: right before such a write the agent
checks the watch folders that contain its paths and right after it records their new state,
but only if the recorded state is still the one from before the write, so a change the user
made earlier still starts a review (a change landing during the write itself is taken for the
agent's). If a check fails, the write counts as a change. When the user's oCIS notification stream reports a file event, the next check comes sooner (it never starts a review itself). A folder that is missing or unreadable is skipped. Paused users are not checked: changes
made while paused start one review after the resume.

### POST /agent-api/v1/proactive/run

Empty body or `{}`. Starts a review now (`trigger_kind` `manual`), regardless of quiet hours and
the interval; it counts as the last scheduled one. `202` with the job (fields as in `GET /jobs`)
and the workers are woken. `409` when it cannot start: `not_enrolled`, `proactive_disabled`,
`kill_switch`, `paused` (`credentials_invalid` for a `credential_*` pause: enrol again),
`live_job`, `daily_cap` or `budget`. Cooldown: 5 s, also used up by a
refused run.

## Audit log

The audit log is append-only and hash-chained per user.

Event object: `{"seq", "ts", "job_id", "kind", "tool", "risk", "args_hash", "cred", "outcome",
"detail"}`. `ts` has millisecond precision; `cred` is the credential the action used (`oidc` for
the user's own requests); `detail` is untrusted (capped at 2 KiB in the list). Kinds:
`action_executed`, `agent_dormant`, `agent_paused`, `agent_resumed`, `approval_acknowledged`,
`approval_decided`, `approval_expired`, `approval_requested`, `chat_message`,
`conversation_deleted`, `job_cancelled`, `job_finished`, `memory_changed`, `memory_edited`,
`policy_changed`, `policy_denied`, `proactive_skipped`, `proactive_started`, `tampered`,
`token_anomaly`, `token_created`, `token_deleted`, `token_expired`, `token_invalid`,
`token_label_closed`, `token_renewed`, `token_revoked_all`, `tool_call`, `unenrolled`. No event holds a token value, hash or label.

### GET /agent-api/v1/audit

Query: `order` (`asc`, the default, by `seq`; or `desc`, newest first), `cursor`, `limit` (1-200),
and the filters `job_id`, `tool`, `risk` (`read`, `write`, `destructive`, `outward`) and `kind` (one
of the kinds above). `{"events": [ … ], "next_cursor": null}`.

The cursor is the `seq` of the last record the previous page covered; a `desc` cursor is prefixed
with `d` and works only in that order. A cursor must name a record of the caller's own log. With
filters, one request scans at most 5000 records: a page may then be short or empty and still carry
a cursor.

### GET /agent-api/v1/audit/verify

Recomputes the caller's chain. No query parameters; `HEAD` is `405`.

```json
{"ok": true, "limitation": "removal of the newest records cannot be detected", "checked": 42}
```

A broken chain is `200` with `"ok": false`, `"message": "the audit chain is broken"` and
`broken_at` (the `seq` of the first failing event); `checked` counts the events that verified.
Removing the newest records cannot be detected. One per user, two in the process.

### GET /agent-api/v1/audit/export

Query `from` (a `seq`, default 1). The whole chain as `application/x-ndjson`
(`Content-Disposition: attachment`), one complete, uncapped record per line with `prev_hash` and
`hash`, so a reader can recompute the chain. `HEAD` is `405`. A failure after the first byte
aborts the connection (the client sees a broken transfer, never a short file that looks complete).
Deadline 5 minutes, each page must be written within 30 s. One per user, eight in the process.

## Events

### GET /agent-api/v1/events

A server-sent event stream (`text/event-stream`, `X-Accel-Buffering: no`) that tells the client
WHAT to refetch, never what changed. No query parameters; `HEAD` is `405`. Frames:

```
event: ready
data: {"v":1}

event: changed
data: {"kinds":["jobs","messages","activity","settings"]}

: ping

event: bye
data: {"reason":"lifetime"}
```

- `ready` first; refetch everything on every `ready`.
- `changed` lists the kinds whose data changed since the last poll (the database is polled every
  2 s, so it works across processes): `jobs`, `messages`, `activity` (approvals, the audit log and
  the status) and `settings`.
- `: ping` every 25 s keeps proxies from closing the connection.
- `bye` ends the stream: `lifetime` (after a random 55 to 65 minutes; reconnect), `shutdown` (the
  sidecar is stopping) or `unavailable` (three failed polls in a row).

There are no `id:` fields and `Last-Event-ID` is ignored. Some changes move no mark (an older
message deleted, the kill switch) and a change in the same millisecond as the previous one can be
missed, so clients also refresh fully about every 60 s, and poll the REST resources after a `429`
or `503` from this route. The bearer is checked only when the stream opens: an open stream outlives
a revoke or an unenrolment by up to 65 minutes (kinds only, for the same user). Streams are capped
per user and per process (`429 rate_limited`, `Retry-After: 5`); while shutting down new streams
get `503 unavailable`.

## Error codes

| Code | Status | Message |
| --- | --- | --- |
| `invalid_request` | 400 | The request is invalid. |
| `unauthenticated` | 401 | Authentication is required. |
| `origin_rejected` | 403 | Cross-origin requests are not allowed. |
| `not_found` | 404 | Not found. |
| `method_not_allowed` | 405 | Method not allowed. |
| `conflict` | 409 | The request conflicts with the current state. |
| `already_exists` | 409 | That file already exists. |
| `precondition_required` | 428 | The If-Match header with the file's ETag is required. |
| `precondition_failed` | 412 | The file changed since you loaded it; reload it. |
| `file_too_large` | 422 | The file is too large to open. |
| `not_text` | 422 | The file is not UTF-8 text. |
| `job_ended` | 409 | The job has already ended. |
| `stale_arguments` | 409 | The action is not the one you were shown; reload it. |
| `already_decided` | 409 | This approval has already been decided. |
| `not_acknowledgeable` | 409 | There is no uncertain outcome to acknowledge for this action. |
| `limit_reached` | 409, 429 | A limit has been reached. |
| `not_enrolled` | 409 | You are not enrolled; enrol first. |
| `budget_exhausted` | 429 | Today's token budget is used up; it resets at midnight UTC. |
| `too_many_pending` | 429 | Too many messages are still waiting for an answer; try again later. |
| `credentials_invalid` | 409 | The agent has no valid credentials for you; enrol again. |
| `proactive_disabled` | 409 | Proactive reviews are turned off in your settings. |
| `kill_switch` | 409 | The operator has stopped the agent. |
| `paused` | 409 | The agent is paused. |
| `live_job` | 409 | A review is already queued, running or waiting for your approval. |
| `daily_cap` | 409 | Today's limit of reviews is reached; it resets at midnight UTC. |
| `budget` | 409 | Most of today's token budget is used; reviews resume at midnight UTC. |
| `expired` | 410 | This has expired. |
| `too_large` | 413 | The request body is too large. |
| `unsupported_media_type` | 415 | The request body must be application/json. |
| `no_login_name` | 422 | Your account has no login name, which the agent needs. |
| `rate_limited` | 429 | Too many requests; try again later. |
| `upstream_rejected` | 502 | ownCloud refused the request. |
| `upstream_unavailable` | 502 | ownCloud could not be reached. |
| `unavailable` | 503 | The service is temporarily unavailable. |
| `internal` | 500 | Internal error. |

`limit_reached` is `409` for a stored limit (200 conversations) and `429`
with `Retry-After` for the daily chat cap. Every `429` carries `Retry-After` in whole seconds.

## Security notes

- Only the user's oCIS bearer authenticates; the sidecar validates it with oCIS on every new token
  and never stores it. Approvals, policy changes, enrolment, revoke and unenrolment always run
  under the user's bearer, never the agent's app token.
- State-changing requests must come from `AGENT_ALLOWED_ORIGIN`; the API sets no CORS headers, so
  serve it on the oCIS origin through the reverse proxy.
- No response carries an app token, a token hash, the master key or an upstream error text.
- Render every untrusted field as plain text.
- Rate limiting by real client must also exist at the reverse proxy: the sidecar sees only the
  proxy's address.
