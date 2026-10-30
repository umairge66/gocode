"""Compare Harbor runs of several agents task by task.

    python3 bench/harbor/compare.py gocode=bench-runs/pilot-gocode,bench-runs/rerun-gocode \
                                    opencode=bench-runs/pilot-opencode,bench-runs/rerun-opencode

For each agent the job folders are read in order; a later folder's trial for
the same task replaces an earlier one (use this for re-runs).
"""

from __future__ import annotations

import json
import sys
from datetime import datetime
from pathlib import Path


def _secs(block: dict | None) -> float | None:
    if not block or not block.get("started_at") or not block.get("finished_at"):
        return None
    f = lambda s: datetime.fromisoformat(s.replace("Z", "+00:00"))
    return (f(block["finished_at"]) - f(block["started_at"])).total_seconds()


def _tool_calls(trial: Path) -> int | None:
    summary = trial / "agent" / "gocode-summary.json"
    if summary.is_file():
        return json.loads(summary.read_text()).get("tool_calls")
    traj = trial / "agent" / "trajectory.json"
    if traj.is_file():
        try:
            data = json.loads(traj.read_text())
        except ValueError:
            return None
        n = 0
        for step in data.get("steps") or []:
            n += len(step.get("tool_calls") or [])
        return n
    return None


def load(folders: list[str]) -> dict[str, dict]:
    trials: dict[str, dict] = {}
    for folder in folders:
        jobs = sorted(p for p in Path(folder).iterdir() if p.is_dir())
        for job in jobs:
            for trial in sorted(p for p in job.iterdir() if p.is_dir() and "__" in p.name):
                task = trial.name.split("__")[0]
                res_path = trial / "result.json"
                if not res_path.is_file():
                    continue
                res = json.loads(res_path.read_text())
                ar = res.get("agent_result") or {}
                reward = None
                rw = trial / "verifier" / "reward.txt"
                if rw.is_file():
                    reward = rw.read_text().strip()
                exc = None
                ex = trial / "exception.txt"
                if ex.is_file():
                    lines = [l for l in ex.read_text().splitlines() if "Error" in l]
                    exc = lines[-1].split(":")[0].split(".")[-1] if lines else "exception"
                trials[task] = {
                    "reward": reward,
                    "exception": exc,
                    "agent_s": _secs(res.get("agent_execution")),
                    "in": ar.get("n_input_tokens"),
                    "cached": ar.get("n_cache_tokens"),
                    "out": ar.get("n_output_tokens"),
                    "cost": ar.get("cost_usd"),
                    "tools": _tool_calls(trial),
                }
    return trials


def main() -> None:
    agents = {}
    for arg in sys.argv[1:]:
        name, _, folders = arg.partition("=")
        agents[name] = load(folders.split(","))
    tasks = sorted({t for a in agents.values() for t in a})
    fmt = lambda v, f="{}": "—" if v is None else f.format(v)
    print("| task | " + " | ".join(f"{a} score | {a} time | {a} tokens in/cached/out | {a} cost | {a} tools" for a in agents) + " |")
    print("|---" * (1 + 5 * len(agents)) + "|")
    totals = {a: {"pass": 0, "n": 0, "cost": 0.0} for a in agents}
    for task in tasks:
        row = [task]
        for a, data in agents.items():
            t = data.get(task) or {}
            score = t.get("reward") or ("⚠ " + t["exception"] if t.get("exception") else "—")
            if t.get("reward") is not None:
                totals[a]["n"] += 1
                totals[a]["pass"] += 1 if t["reward"] in ("1", "1.0") else 0
            totals[a]["cost"] += t.get("cost") or 0
            row += [str(score), fmt(t.get("agent_s"), "{:.0f}s"),
                    f"{fmt(t.get('in'))}/{fmt(t.get('cached'))}/{fmt(t.get('out'))}",
                    fmt(t.get("cost"), "${:.3f}"), fmt(t.get("tools"))]
        print("| " + " | ".join(row) + " |")
    for a, s in totals.items():
        print(f"{a}: passed {s['pass']}/{s['n']} graded tasks, reported cost ${s['cost']:.3f}")


if __name__ == "__main__":
    main()
