"""Harbor's Claude Code agent with a local install cache.

Harbor reinstalls Claude Code in every fresh task container (apt nodejs/npm,
then the official native installer). On a slow connection that is 10+ minutes
per task. This subclass installs once the normal way, copies the resulting
self-contained `claude` binary to a cache folder on the host, and on later
runs uploads that binary instead of downloading anything.

Usage (from the goport folder):

    PYTHONPATH=bench/harbor harbor run ... --agent-import-path claude_cached:CachedClaudeCode

Cache: $CLAUDE_CODE_CACHE_DIR, default ~/Downloads/go-code/bench-runs/cache/claude.
Delete the folder to force a fresh install (e.g. to pick up a new version).
"""

from __future__ import annotations

import os
from pathlib import Path

from harbor.agents.installed.claude_code import ClaudeCode

CACHE_DIR = Path(os.environ.get("CLAUDE_CODE_CACHE_DIR") or Path.home() / "Downloads/go-code/bench-runs/cache/claude")
CACHED_BIN = CACHE_DIR / "claude"


class CachedClaudeCode(ClaudeCode):
    """Claude Code, installed from a host-side cache after the first run."""

    @staticmethod
    def name() -> str:
        return "claude-code-cached"

    async def _home(self, environment) -> str:
        res = await self.exec_as_agent(environment, command='printf %s "$HOME"')
        home = (getattr(res, "stdout", "") or "").strip()
        return home or "/root"

    async def install(self, environment) -> None:
        home = await self._home(environment)
        target = f"{home}/.local/bin/claude"
        if CACHED_BIN.is_file():
            await self.exec_as_agent(environment, command=f'mkdir -p "{home}/.local/bin"')
            await environment.upload_file(CACHED_BIN, target)
            await self.exec_as_root(environment, command=f'chmod 755 "{target}"')
            await self.exec_as_agent(
                environment,
                command=(
                    "echo 'export PATH=\"$HOME/.local/bin:$PATH\"' >> ~/.bashrc && "
                    'export PATH="$HOME/.local/bin:$PATH" && claude --version'
                ),
            )
            return

        # First run: the normal install, then keep the binary for next time.
        await super().install(environment)
        res = await self.exec_as_agent(
            environment, command='export PATH="$HOME/.local/bin:$PATH"; readlink -f "$(command -v claude)"'
        )
        real = (getattr(res, "stdout", "") or "").strip()
        if real:
            CACHE_DIR.mkdir(parents=True, exist_ok=True)
            tmp = CACHE_DIR / "claude.partial"
            await environment.download_file(real, tmp)
            if tmp.is_file() and tmp.stat().st_size > 1_000_000:
                tmp.chmod(0o755)
                tmp.replace(CACHED_BIN)
            else:
                tmp.unlink(missing_ok=True)
