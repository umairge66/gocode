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
| `{"type":"result","subtype":"success"\|"error_max_turns"\|"error_during_execution"\|"error_max_structured_output_retries",…}` | The end of each turn: `result` (or `errors`), `is_error`, `num_turns`, `duration_ms`, `usage`, `modelUsage`, `total_cost_usd`, `permission_denials`, `stop_reason`. |

A subagent's own messages (its `assistant` records and the `user` records of
its tool results) are forwarded too, as they happen, with
`parent_tool_use_id` set to the id of the Agent tool call that spawned it (a
nested subagent's records name the nested Agent call). A subagent resumed by
a SendMessage keeps the id of its original Agent call. The session's own
records carry `parent_tool_use_id: null`. The subagent's prompt itself is
not repeated as a record; it is the Agent call's input.

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

| Subtype | Request fields | Response |
| :- | :- | :- |
| `initialize` | (SDK options such as `hooks` are accepted and ignored; hooks come from settings) | `commands` (`name`, `description`, `argumentHint`), `output_style`, `available_output_styles`, `models`, `account`, `capabilities`, `pid` |
| `interrupt` | — | `{}`. Ends the running turn as if the user pressed Esc: its result is an `error_during_execution` with the error `Request interrupted by user`, and the session takes the next message. A no-op between turns. |
| `set_model` | `model` (`"default"` or empty: the model the session started with) | `{"model":"…"}`. Applies from the next turn; a request sent during a turn is answered when the turn ends. The model policy (`availableModels`) can refuse it. |
| `set_permission_mode` | `mode`: `default`, `acceptEdits`, `plan`, `dontAsk`, `auto`, `bypassPermissions` | `{"mode":"…"}`. Takes effect at the next tool call, also mid-turn. `bypassPermissions` needs a session started with `--dangerously-skip-permissions` or `--allow-dangerously-skip-permissions` (and not disabled by settings); `auto` needs auto mode to be available. |
| `mcp_status` | — | `{"mcpServers":[{"name":"…","status":"connected"\|"pending"\|"failed"\|"needs-auth","serverInfo":{"name","version"},"error":"…"}]}` |
| `get_context_usage` | — | `totalTokens`, `maxTokens`, `rawMaxTokens`, `percentage`, `model`, `isAutoCompactEnabled`, `autoCompactThreshold`, `categories` (one `Messages` entry), and empty `memoryFiles` / `mcpTools` / `agents` / `gridRows`. During a turn it reports the size the latest model response measured. |

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

The daemon's `permission-response` op is accepted (with the control key) but
not routed anywhere yet: background sessions run the interactive UI, which
asks its permission questions in the attached terminal.
