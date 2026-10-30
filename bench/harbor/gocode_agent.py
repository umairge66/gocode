"""Harbor adapter for gocode (Terminal-Bench 2.0).

Usage (from the goport folder):

    PYTHONPATH=bench/harbor harbor run -d terminal-bench@2.0 \
        --agent-import-path gocode_agent:GocodeAgent \
        -m openrouter/<model id> -k 5 ...

The agent copies the locally built Linux binary (./gocode, or $GOCODE_BINARY)
into the task container and runs it non-interactively on the task
instruction. The model is "<provider>/<model id>" (provider: openrouter,
anthropic, openai, groq, gemini, …). The provider API key comes from the host
environment, or from gocode's own auth.json when it is not set there; it is
passed to the container process only, never logged.
"""

from __future__ import annotations

import json
import os
import shlex
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent

REMOTE_BIN = "/installed-agent/gocode"
EXIT_MARKER = "__GOCODE_EXIT_CODE="
REMOTE_LOGS = "/logs/agent"
REMOTE_STREAM = f"{REMOTE_LOGS}/gocode-stream.jsonl"

# Provider id -> the API-key variable gocode reads for it.
KEY_VARS = {
    "openrouter": "OPENROUTER_API_KEY",
    "anthropic": "ANTHROPIC_API_KEY",
    "openai": "OPENAI_API_KEY",
    "gemini": "GEMINI_API_KEY",
    "groq": "GROQ_API_KEY",
    "cerebras": "CEREBRAS_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "xai": "XAI_API_KEY",
    "zai": "ZAI_API_KEY",
    "moonshot": "MOONSHOT_API_KEY",
}


def _auth_json_key(provider: str) -> str:
    """The key gocode saved for provider with /login, or ""."""
    home = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    try:
        data = json.loads((home / "gocode" / "auth.json").read_text())
    except (OSError, ValueError):
        return ""
    return (data.get("apiKeys") or {}).get(provider, "") or ""


class GocodeAgent(BaseInstalledAgent):
    """Runs gocode headlessly (`gocode -p`) inside a Harbor task container."""

    @staticmethod
    def name() -> str:
        return "gocode"

    def get_version_command(self) -> str | None:
        return f"{REMOTE_BIN} --version"

    def _split_model(self) -> tuple[str, str]:
        if not self.model_name or "/" not in self.model_name:
            raise ValueError("model must be <provider>/<model id>, e.g. openrouter/deepseek/deepseek-v4-flash")
        provider, model = self.model_name.split("/", 1)
        return provider, model

    async def install(self, environment) -> None:
        binary = Path(os.environ.get("GOCODE_BINARY") or Path(__file__).resolve().parents[2] / "gocode")
        if not binary.is_file():
            raise RuntimeError(f"gocode binary not found at {binary} (build it with: go build -o gocode ./cmd/gocode)")
        await environment.upload_file(binary, REMOTE_BIN)
        await self.exec_as_root(environment, command=f"chmod 755 {REMOTE_BIN}")

    async def run(self, instruction, environment, context) -> None:
        provider, model = self._split_model()
        key_var = KEY_VARS.get(provider, provider.upper().replace("-", "_") + "_API_KEY")
        key = os.environ.get(key_var) or _auth_json_key(provider)
        if not key:
            raise RuntimeError(f"no API key for {provider}: set {key_var} or run /login in gocode")

        env = {
            key_var: key,
            "GOCODE_PROVIDER": provider,
            # Containers run as root and nobody can answer prompts: allow
            # bypass mode there (gocode refuses it as root otherwise).
            "IS_SANDBOX": "1",
            "HOME": "/root",
            "NO_COLOR": "1",
        }
        cmd = (
            f"{REMOTE_BIN} -p {shlex.quote(self.render_instruction(instruction))} "
            f"--model {shlex.quote(model)} --dangerously-skip-permissions "
            "--output-format stream-json --verbose"
        )
        # The stream goes to /logs/agent as it is produced: Harbor copies that
        # folder back even when the task's time limit stops the agent. gocode
        # exits non-zero when a run ends with an error (API failure, loop
        # guard stop); the command's own status stays 0 so Harbor still
        # returns the output, and gocode's code is recorded in the logs.
        cmd = (
            f"mkdir -p {REMOTE_LOGS} && {cmd} > {REMOTE_STREAM}; code=$?; "
            f"cat {REMOTE_STREAM}; echo \"{EXIT_MARKER}$code\" >&2"
        )
        result = await self.exec_as_agent(environment, command=cmd, env=env)
        out = getattr(result, "stdout", "") or ""
        err = getattr(result, "stderr", "") or ""
        self.logs_dir.mkdir(parents=True, exist_ok=True)
        (self.logs_dir / "gocode-stream.jsonl").write_text(out)
        summary = summarize_stream(out)
        (self.logs_dir / "gocode-output.txt").write_text(
            (summary.get("result") or "") + ("\n--- stderr ---\n" + err if err else "")
        )
        (self.logs_dir / "gocode-summary.json").write_text(json.dumps(summary, indent=2))

    def populate_context_post_run(self, context) -> None:
        path = self.logs_dir / "gocode-summary.json"
        stream = self.logs_dir / "gocode-stream.jsonl"
        if not path.is_file() and stream.is_file():
            # The run was stopped before run() returned (time limit): build
            # the summary from the partial stream Harbor copied back.
            path.write_text(json.dumps(summarize_stream(stream.read_text()), indent=2))
        if not path.is_file():
            return
        s = json.loads(path.read_text())
        context.n_input_tokens = s.get("input_tokens_total") or 0
        context.n_output_tokens = s.get("output_tokens") or 0
        context.n_cache_tokens = s.get("cache_read_tokens") or 0
        if s.get("cost_usd"):
            context.cost_usd = s["cost_usd"]


def summarize_stream(text: str) -> dict:
    """Totals from gocode's stream-json output: tokens (input counted WITH
    cached tokens, like other agents report it), tool calls, turns, cost."""
    out = {"tool_calls": 0, "input_tokens_total": 0, "output_tokens": 0,
           "cache_read_tokens": 0, "cost_usd": None, "num_turns": None,
           "is_error": None, "subtype": None, "result": ""}
    partial = {"in": 0, "cached": 0, "out": 0}
    seen_result = False
    for line in text.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            rec = json.loads(line)
        except ValueError:
            continue
        if rec.get("type") == "assistant":
            msg = rec.get("message") or {}
            for block in msg.get("content") or []:
                if block.get("type") == "tool_use":
                    out["tool_calls"] += 1
            # Per-message usage: the totals when the run ended before its
            # result record (replaced by the result record when there is one).
            u = msg.get("usage") or {}
            cache_read = u.get("cache_read_input_tokens") or 0
            partial["in"] += (u.get("input_tokens") or 0) + cache_read + (u.get("cache_creation_input_tokens") or 0)
            partial["cached"] += cache_read
            partial["out"] += u.get("output_tokens") or 0
        elif rec.get("type") == "result":
            seen_result = True
            u = rec.get("usage") or {}
            cache_read = u.get("cache_read_input_tokens") or 0
            out["input_tokens_total"] = (u.get("input_tokens") or 0) + cache_read + (u.get("cache_creation_input_tokens") or 0)
            out["cache_read_tokens"] = cache_read
            out["output_tokens"] = u.get("output_tokens") or 0
            out["cost_usd"] = rec.get("total_cost_usd")
            out["num_turns"] = rec.get("num_turns")
            out["is_error"] = rec.get("is_error")
            out["subtype"] = rec.get("subtype")
            out["result"] = rec.get("result") or "; ".join(rec.get("errors") or [])
    if not seen_result:
        out["input_tokens_total"] = partial["in"]
        out["cache_read_tokens"] = partial["cached"]
        out["output_tokens"] = partial["out"]
        out["subtype"] = "incomplete"
    return out
