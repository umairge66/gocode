# Headless mode (`gocode -p`)

`gocode -p` runs without the terminal UI. It reads the prompt from its
argument (or piped stdin), runs it, and prints the outcome in one of three
formats. A run saves a transcript like an interactive session, so it can be
continued with `--resume` / `--continue` (unless `--no-session-persistence`).

```sh
gocode -p "summarize the changes on this branch"                 # text
gocode -p "fix the failing test" --output-format json            # one result record
gocode -p "…" --output-format stream-json --verbose              # event stream
gocode -p --input-format stream-json --output-format stream-json --verbose   # a host drives it
```

The record formats follow the Claude Code `stream-json` protocol (the one the
Claude Agent SDK speaks), so an SDK-style host can drive gocode the same way.

| Flag | Meaning |
| :- | :- |
| `--output-format text\|json\|stream-json` | Final text; one `result` record (with `--verbose`, a JSON array of every record); or newline-delimited records as they happen (needs `--verbose`). |
| `--input-format text\|stream-json` | `stream-json` reads user messages and control requests on stdin (needs `--output-format stream-json`). |
| `--include-partial-messages` | Adds `stream_event` records with the model's streaming events (stream-json output only). |
| `--json-schema <schema>` | The answer is an object validated against the schema, in the result's `structured_output`. |
| `--max-turns <n>` | Ends a turn after n model rounds (`error_max_turns`). |
| `--permission-prompt-tool <tool>` | Who decides a permission prompt: an MCP tool (`mcp__server__tool`), or `stdio` for the stream-json host (requires `--input-format stream-json`). Without this flag a permission prompt is denied. |

## Output records

Every record is one line of JSON with a `type`; every record except
`control_request` / `control_response` / `control_cancel_request` carries the
`session_id` and a `uuid`.

| Record | When |
| :- | :- |
| `{"type":"system","subtype":"init",…}` | First. The session's cwd, model, `permissionMode`, `tools`, `mcp_servers`, `slash_commands`, `apiKeySource`, and `capabilities` (below). |
| `{"type":"assistant","message":{…},"parent_tool_use_id":null}` | Each model response; `message` is a Messages API message (`content`, `stop_reason`, `usage`). |
| `{"type":"user","message":{"role":"user","content":[tool_result…]}}` | The tool results of each round. |
| `{"type":"stream_event","event":{…}}` | With `--include-partial-messages`: the streaming events of the response being written (see below). |
| `{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto"\|"manual","pre_tokens":N}}` | A compaction rebuilt the history (automatic, `/compact`, or the recovery from a full context window). |
| `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":1200,"error_status":529,"error":"…"}` | A model request failed with a transient error and is retried after the delay. `error_status` is null for a connection error. |
| `{"type":"system","subtype":"task_started","task_id":"…","task_type":"local_bash"\|"local_agent","description":"…","tool_use_id":"…","is_backgrounded":true}` | A background command (`run_in_background`) or a subagent started. A subagent also carries `spawn_depth` and `parent_task_id` when nested. |
| `{"type":"system","subtype":"task_notification","task_id":"…","status":"completed"\|"failed"\|"stopped","output_file":"…","summary":"…"}` | That task ended. |
| `{"type":"result","subtype":"success"\|"error_max_turns"\|"error_max_budget_usd"\|"error_during_execution"\|"error_max_structured_output_retries",…}` | The end of each turn: `result` (or `errors`), `is_error`, `num_turns`, `duration_ms`, `usage`, `modelUsage`, `total_cost_usd`, `permission_denials`, `stop_reason`. |

A subagent's own messages (its `assistant` records and the `user` records of
its tool results) are forwarded too, as they happen, with
`parent_tool_use_id` set to the id of the Agent tool call that spawned it (a
nested subagent's records name the nested Agent call). A subagent resumed by
a SendMessage keeps the id of its original Agent call. The session's own
records carry `parent_tool_use_id: null`. The subagent's prompt itself is
not repeated as a record; it is the Agent call's input.

When a PreToolUse hook returns `permissionDecision: "defer"`, the turn ends
without running the call: the result is a `success` with `stop_reason` and
`terminal_reason` `"tool_deferred"` and the call in
`deferred_tool_use: {"id","name","input"}`. `gocode -p --resume <session>`
(or `-c`), with no prompt, asks the hooks and permissions again and runs the
call; the conversation continues from its result. A resume with a prompt
settles the call first (a hook that defers again then gets an ask). If the
tool is gone, the result has `stop_reason` `"tool_deferred_unavailable"`,
`is_error: true`, and the call stays pending.

### Partial messages

With `--include-partial-messages`, each model response is framed as in the
Messages API stream: `message_start`; for every content block a
`content_block_start`, its `content_block_delta` events and a
`content_block_stop`; then `message_delta` (stop reason, output tokens) and
`message_stop`. Blocks are numbered from 0 within the response.

| Block | `content_block_start` | Deltas |
| :- | :- | :- |
| thinking | `{"type":"thinking","thinking":"","signature":""}` | `thinking_delta` |
| text | `{"type":"text","text":""}` | `text_delta` |
| tool_use | `{"type":"tool_use","id":"…","name":"…","input":{}}` | `input_json_delta` (`partial_json` fragments that join into the input) |

The Anthropic provider (and providers built on it) streams all three. The
OpenAI-compatible providers stream text and thinking; their tool calls appear
only in the `assistant` record.

## Stream-json input

With `--input-format stream-json`, stdin carries one JSON record per line and
is read while turns run. The session ends when stdin closes (after the
running turn, and after any background commands still running are awaited).
A line that is not JSON ends the run with an error.

### User messages

```json
{"type":"user","message":{"role":"user","content":"Fix the build"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"What is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR…"}}]}}
```

Content is a string or a list of `text` and `image` blocks (`source` of type
`base64`, or `url` where the provider can fetch it). A model that cannot take
images gets a note in place of the image.

Each message runs as a turn and gets its own `result`. A message that arrives
while a turn runs is queued: at the turn's next tool round the queued messages
(up to the first slash command) are given to the model with the tool results,
so it can take them into account at once; whatever is still queued when the
turn ends runs as the next turn. A slash command always waits for its own
turn.

### Control requests

The host sends

```json
{"type":"control_request","request_id":"req_1","request":{"subtype":"interrupt"}}
```

and gocode answers on stdout with

```json
{"type":"control_response","response":{"subtype":"success","request_id":"req_1","response":{}}}
{"type":"control_response","response":{"subtype":"error","request_id":"req_1","error":"…"}}
```

`system/init` lists the subtypes the session accepts in `capabilities`, so a
host can check before sending one. An unknown subtype is answered with an
error.

`initialize`'s `models` lists `default` (the model the session started
with) and, for the Anthropic and OpenRouter providers, the Claude family
models (`value` is the full model id, which `set_model` takes; `displayName`,
`description`, `resolvedModel`). Other providers list their model catalog,
fetched from the provider when `initialize` is answered (within 5 s; a
failed fetch lists only `default`). The `availableModels` policy filters the
list as it does the `/model` picker. `account` is
`{"provider":"…","apiKeySource":"…"}`: gocode has no account, and
`apiKeySource` names where the key came from (the environment variable's
name, `auth.json` for a saved key, `settings`, `apiKeyHelper`, or `none`),
never the key.

| Subtype | Request fields | Response |
| :- | :- | :- |
| `initialize` | (SDK options such as `hooks` are accepted and ignored; hooks come from settings) | `commands` (`name`, `description`, `argumentHint`), `output_style`, `available_output_styles`, `models`, `account`, `capabilities`, `pid` (see below) |
| `interrupt` | — | `{}`. Ends the running turn as if the user pressed Esc: its result is an `error_during_execution` with the error `Request interrupted by user`, and the session takes the next message. A no-op between turns. |
| `set_model` | `model` (`"default"` or empty: the model the session started with) | `{"model":"…"}`. Applies from the next turn; a request sent during a turn is answered when the turn ends. The model policy (`availableModels`) can refuse it. |
| `set_permission_mode` | `mode`: `default`, `acceptEdits`, `plan`, `dontAsk`, `auto`, `bypassPermissions` | `{"mode":"…"}`. Takes effect at the next tool call, also mid-turn. `bypassPermissions` needs a session started with `--dangerously-skip-permissions` or `--allow-dangerously-skip-permissions` (and not disabled by settings); `auto` needs auto mode to be available. |
| `mcp_status` | — | `{"mcpServers":[{"name":"…","status":"connected"\|"pending"\|"failed"\|"needs-auth","serverInfo":{"name","version"},"error":"…"}]}` |
| `get_context_usage` | — | `totalTokens`, `maxTokens`, `rawMaxTokens`, `percentage`, `model`, `isAutoCompactEnabled`, `autoCompactThreshold`, `categories`, `memoryFiles` (`path`, `type`, `tokens`), `mcpTools` (`name`, `serverName`, `tokens`), and empty `agents` / `gridRows`. Between turns `categories` breaks the total down into `system_prompt`, `system_tools`, `mcp_tools`, `memory_files`, `messages` and `free_space` (`id`, `name`, `tokens`, `color`, `kind`); the parts are ≈4-characters-per-token estimates, as in `/context`, and `messages` is what the measured total leaves after them. During a turn it reports the size the latest model response measured, as one `messages` category. |

### Permission prompts (`can_use_tool`)

When the permission rules leave a tool call to the user and the session was
started with `--input-format stream-json --permission-prompt-tool stdio`,
gocode asks the host instead of refusing the call:

```json
{"type":"control_request","request_id":"gocode_req_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make test"},"tool_use_id":"toolu_…","decision_reason":"…","permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"make:*"}],"behavior":"allow","destination":"localSettings"}]}}
```

The host answers with a `control_response` for that `request_id` whose
`response` is the decision:

```json
{"type":"control_response","response":{"subtype":"success","request_id":"gocode_req_1","response":{"behavior":"allow","updatedInput":{"command":"make test"}}}}
{"type":"control_response","response":{"subtype":"success","request_id":"gocode_req_1","response":{"behavior":"deny","message":"Not on this branch"}}}
```

- `allow` runs the call. `updatedPermissions` may carry an `addRules` entry
  (behavior `allow`), which allows matching calls for the rest of the
  session, or `setMode` `acceptEdits` for an edit, which switches the session
  to acceptEdits. Rules are not written to settings files, whatever their
  `destination`. An `updatedInput` that differs from the call's input (it
  must be an object) is the input the call runs with. It is checked again as
  a PreToolUse hook's rewrite is: the permission fields come from the new
  input, so a deny rule (or the working-directory guard) still refuses it;
  a check that would ask again is answered by this allow.
- `deny` refuses the call with `message`, and the model goes on. With
  `"interrupt": true` (or no message) the turn ends there, as when the user
  rejects a prompt without feedback.
- An `error` response, or stdin closing, refuses the call.

While gocode waits, the host can keep sending messages and control requests.
If the turn ends first (an `interrupt`), gocode withdraws the request with
`{"type":"control_cancel_request","request_id":"gocode_req_1"}`.

Prompts are asked one at a time, also for subagents. The host has to answer
every `can_use_tool`, or the turn waits.

### `--permission-prompt-tool`

`--permission-prompt-tool mcp__server__tool` has an MCP tool decide instead
(with any input format). The tool is called with
`{"tool_name":"…","input":{…},"tool_use_id":"…"}` and must return, as text, the
same JSON decision as above (`{"behavior":"allow"}` or
`{"behavior":"deny","message":"…"}`). A failed call, or an answer that is not
a decision, refuses the tool call. `--permission-prompt-tool stdio` names the
stream-json host; it is opt-in, as in Claude Code, so a host that does not
answer `can_use_tool` never leaves a turn waiting.

## Background sessions

Background sessions run the interactive UI in the daemon's pty, so their
permission prompts, AskUserQuestion dialogs and plan approvals open in the
attached terminal. A daemon client can answer them too:

- While one is open the job is `blocked` (its `needs` names the prompt),
  and the daemon's `list` reply carries it on the job's record:
  `"permission":{"requestId":"perm_…","kind":"permission","toolName":"Bash","input":{…},"toolUseId":"…","reason":"…","suggestions":["Bash(make:*)"],"at":"…"}`.
  `kind` is `permission` (a tool call), `question` (AskUserQuestion;
  `input` is `{"questions":[{question, header, options:[{label, description}], multiSelect}]}`)
  or `plan` (ExitPlanMode's approval; `input` is
  `{"plan":"…","planFilePath":"…","modes":["acceptEdits","default"]}`, the
  modes an approval can continue in). A record without `kind` is a
  permission prompt.
- The authed `permission-response` op answers it:
  `{"op":"permission-response","auth":"<control key>","short":"<job>","requestId":"perm_…","decision":{"behavior":"allow"}}`.
  For a permission prompt `decision` is the same decision as a `can_use_tool`
  answer and is applied the same way (`updatedInput`, session-only
  `addRules`, `setMode` `acceptEdits`; `deny` with `message` / `interrupt`).
  For a question, `allow` carries the answers as a `can_use_tool` host gives
  them, `{"behavior":"allow","updatedInput":{"answers":{"<question>":"<label>"}}}`
  (a list of labels for a multi-select question; a question left out is
  answered "(notes only)"), and `deny` dismisses the dialog. For a plan,
  `allow` approves — in the mode of a `setMode` update when the dialog offers
  it, else in the default mode — and `deny` keeps planning, `message` being
  what to change. The dialog in the terminal closes.
- The op fails with `ENOREPLY` when the job has no open prompt, `ESTALE` when
  `requestId` names one that is no longer open (an empty `requestId`
  answers whichever is open), and `EPROTO` for a decision that is not
  `allow` / `deny`. Whichever answer comes first — the terminal's or the
  client's — wins; a client's answer that loses that race is dropped.

The prompt and the answer pass through the job directory
(`permission.json`, `permission-response.json`, mode 0600), and the session
checks for an answer every 200 ms while the prompt is open. One prompt is
offered at a time: one that opens while another is offered (a subagent's
permission prompt during a question, say) is answered in the terminal only.

From the command line:

```sh
gocode agents pending [--json]          # jobs waiting, with kind, tool, summary, reason, request id
gocode agents respond <job> allow [--always]          # --always: also add the suggested rule, for this session
gocode agents respond <job> deny [--message "<why>"]
gocode agents respond <job> --answer "<option label>"  # a question; one --answer per question
gocode agents respond <job> allow [--mode acceptEdits] # a plan approval
```

`<job>` is the id (or a prefix, or part of the session's name).
`respond` answers the job's open prompt with its request id, so an answer
for a prompt that has since been answered in the terminal or replaced is
refused rather than applied to the next one.
