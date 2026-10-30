"""Harbor's OpenCode agent with a local install cache.

Harbor reinstalls OpenCode in every fresh task container (apt nodejs/npm, nvm
Node, then `npm i -g opencode-ai`). On a slow connection that takes many
minutes per task. This subclass installs once the normal way, copies the
self-contained native `opencode` binary from the npm package to a cache
folder on the host, and on later runs uploads that binary instead.

Usage (from the goport folder):

    PYTHONPATH=bench/harbor harbor run ... --agent-import-path opencode_cached:CachedOpenCode

Cache: $OPENCODE_CACHE_DIR, default ~/Downloads/go-code/bench-runs/cache/opencode.
Delete the folder to force a fresh install (e.g. to pick up a new version).
"""

from __future__ import annotations

import os
from pathlib import Path

from harbor.agents.installed.opencode import OpenCode

CACHE_DIR = Path(os.environ.get("OPENCODE_CACHE_DIR") or Path.home() / "Downloads/go-code/bench-runs/cache/opencode")
CACHED_BIN = CACHE_DIR / "opencode"
TARGET = "/usr/local/bin/opencode"

# The native binary inside the npm package (glibc x64 build first).
FIND_NATIVE = (
    "for f in $(find / -xdev -type f -name opencode -path '*opencode-linux-x64*' 2>/dev/null); do "
    "case \"$f\" in *musl*) continue;; esac; echo \"$f\"; break; done"
)


class CachedOpenCode(OpenCode):
    """OpenCode, installed from a host-side cache after the first run."""

    @staticmethod
    def name() -> str:
        return "opencode-cached"

    async def install(self, environment) -> None:
        if CACHED_BIN.is_file():
            await environment.upload_file(CACHED_BIN, TARGET)
            await self.exec_as_root(environment, command=f"chmod 755 {TARGET} && {TARGET} --version")
            return

        # First run: the normal install, then keep the native binary.
        await super().install(environment)
        res = await self.exec_as_root(environment, command=FIND_NATIVE)
        native = (getattr(res, "stdout", "") or "").strip()
        if native:
            CACHE_DIR.mkdir(parents=True, exist_ok=True)
            tmp = CACHE_DIR / "opencode.partial"
            await environment.download_file(native, tmp)
            if tmp.is_file() and tmp.stat().st_size > 1_000_000:
                tmp.chmod(0o755)
                tmp.replace(CACHED_BIN)
            else:
                tmp.unlink(missing_ok=True)
