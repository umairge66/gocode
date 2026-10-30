#!/usr/bin/env python3
"""Replay recorded gocode benchmark runs under the current context limits.

For each task run (a gocode stream-json log), this estimates how many input
tokens the run would have used with:

  1. the smaller per-request overhead (fewer tool definitions offered in -p:
     no AskUserQuestion, no browser tools without --chrome);
  2. the inline tool-result limits (Bash 30000 chars, Grep 20000, Read's
     25000-token budget, 50000 for everything else) — an oversized result is
     replaced by a ~2.3KB saved-output preview;
  3. calls that can no longer fail or happen: Bash/Read/Grep numeric inputs
     sent as whole-number floats (timeout: 120000.0) are accepted now, and
     AskUserQuestion is not offered in -p, so those turns and their retries
     disappear.

Each request's recorded input_tokens is the prompt size the provider counted.
A removed turn drops the retry request it caused and shrinks every later
request by the growth that turn added; a shrunk result shrinks every later
request by its estimated token saving; the overhead saving applies to every
request.

Usage:
  python3 bench/replay_limits.py <pilot-gocode-dir> [<pilot-opencode-dir>]
      [--overhead-before CHARS] [--overhead-after CHARS]

The overhead sizes default to the measured first-request system prompt plus
tool definitions of a -p session before and after the change.
"""

from __future__ import annotations

import argparse
import glob
import json
import os
import re

OVERHEAD_BEFORE = 17315 + 59633  # system prompt + 29 tool definitions (chars)
OVERHEAD_AFTER = 17315 + 49471   # system prompt + 17 tool definitions (chars)

BASH_LIMIT = 30000
GREP_LIMIT = 20000
DEFAULT_LIMIT = 50000
READ_TOKENS = 25000
PERSISTED_PREVIEW_CHARS = 2350  # frame + 2000-char preview


def text_of(content) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(c.get("text", "") for c in content if isinstance(c, dict))
    return ""


def capped_len(tool: str, text: str, tool_input: dict) -> int:
    """Length of a recorded result under the current inline limits."""
    n = len(text)
    if tool == "Read":
        ext = os.path.splitext(tool_input.get("file_path", ""))[1].lower()
        per_token = 2 if ext in (".json", ".jsonl", ".jsonc") else 4
        # numbered output: strip the "N\t" prefixes for the estimate
        raw = re.sub(r"(?m)^\d+\t", "", text)
        if len(raw) / per_token > READ_TOKENS:
            return int(READ_TOKENS * per_token * 0.85)
        return n
    limit = {"Bash": BASH_LIMIT, "Grep": GREP_LIMIT}.get(tool, DEFAULT_LIMIT)
    return PERSISTED_PREVIEW_CHARS if n > limit else n


def avoidable(tool: str, result: str, is_error: bool) -> str | None:
    """Why a recorded call would not happen under the current build."""
    if is_error and re.search(r"cannot unmarshal number [0-9.eE+-]+ into Go struct field \w+\.\w+ of type int", result):
        return "whole-number float input"
    if tool == "AskUserQuestion":
        return "AskUserQuestion in -p"
    return None


def replay(path: str, saved_overhead_chars: int) -> dict:
    turns = []  # per assistant message: input tokens, tool calls
    calls = {}
    results = {}  # tool_use_id -> (text, is_error)
    final_usage = None
    for line in open(path, encoding="utf-8", errors="replace"):
        try:
            o = json.loads(line)
        except ValueError:
            continue
        if o.get("type") == "assistant":
            msg = o["message"]
            uses = [c for c in msg.get("content", []) if c.get("type") == "tool_use"]
            for u in uses:
                calls[u["id"]] = (u["name"], u.get("input") or {})
            turns.append({"input": msg["usage"]["input_tokens"], "uses": [u["id"] for u in uses],
                          "chars": sum(len(json.dumps(c)) for c in msg.get("content", []))})
        elif o.get("type") == "user" and isinstance(o["message"].get("content"), list):
            for c in o["message"]["content"]:
                if c.get("type") == "tool_result":
                    results[c["tool_use_id"]] = (text_of(c.get("content")), bool(c.get("is_error")))
        elif o.get("type") == "result":
            final_usage = o.get("usage")

    n = len(turns)
    inputs = [t["input"] for t in turns]
    first_ratio = OVERHEAD_BEFORE / max(1, inputs[0]) if inputs else 4.0
    overhead_tokens = saved_overhead_chars / first_ratio

    dropped = set()       # request indices that would not be sent
    shrink = [0.0] * n    # tokens removed from request i onward (cumulative later)
    reasons = {}
    result_saving = 0.0
    for k, t in enumerate(turns):
        if k + 1 >= n:
            continue
        growth = inputs[k + 1] - inputs[k]
        res_chars = sum(len(results.get(u, ("", False))[0]) for u in t["uses"])
        for u in t["uses"]:
            name, inp = calls[u]
            text, is_err = results.get(u, ("", False))
            why = avoidable(name, text, is_err)
            if why and len(t["uses"]) == 1:
                reasons[why] = reasons.get(why, 0) + 1
                dropped.add(k + 1)
                for j in range(k + 2, n):
                    shrink[j] += growth
                continue
            new_len = capped_len(name, text, inp)
            if new_len < len(text) and res_chars:
                tokens = growth * len(text) / (res_chars + t["chars"])
                saving = tokens * (1 - new_len / len(text))
                result_saving += saving
                for j in range(k + 1, n):
                    shrink[j] += saving

    new_total = 0.0
    for i in range(n):
        if i in dropped:
            continue
        new_total += max(0.0, inputs[i] - shrink[i] - overhead_tokens)
    return {
        "requests": n,
        "recorded_input": sum(inputs),
        "final_usage_input": (final_usage or {}).get("input_tokens"),
        "first_request": inputs[0] if inputs else 0,
        "overhead_saving_per_request": round(overhead_tokens),
        "dropped_requests": len(dropped),
        "dropped_reasons": reasons,
        "result_cap_tokens_saved_per_later_request": round(result_saving),
        "estimated_input": round(new_total),
        "estimated_requests": n - len(dropped),
    }


def opencode_totals(task_dir: str) -> dict | None:
    f = os.path.join(task_dir, "agent", "opencode.txt")
    if not os.path.isfile(f):
        return None
    steps = []
    for line in open(f, encoding="utf-8", errors="replace"):
        try:
            o = json.loads(line)
        except ValueError:
            continue
        if o.get("type") == "step_finish":
            t = o["part"]["tokens"]
            steps.append(t["input"] + t["cache"]["read"] + t["cache"]["write"])
    return {"requests": len(steps), "input": sum(steps), "first_request": steps[0] if steps else 0}


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("gocode_dir")
    ap.add_argument("opencode_dir", nargs="?")
    ap.add_argument("--overhead-before", type=int, default=OVERHEAD_BEFORE)
    ap.add_argument("--overhead-after", type=int, default=OVERHEAD_AFTER)
    args = ap.parse_args()

    oc = {}
    if args.opencode_dir:
        for d in glob.glob(os.path.join(args.opencode_dir, "**", "*__*"), recursive=True):
            if os.path.isdir(d):
                tot = opencode_totals(d)
                if tot:
                    oc[os.path.basename(d).split("__")[0]] = tot

    hdr = f"{'task':22s} {'req':>4s} {'recorded':>10s} {'est. now':>10s} {'saved':>6s} {'drop':>4s} {'ovh/req':>7s}  {'opencode':>10s} {'oc req':>6s}"
    print(hdr)
    print("-" * len(hdr))
    for f in sorted(glob.glob(os.path.join(args.gocode_dir, "**", "gocode-stream.jsonl"), recursive=True)):
        task = os.path.basename(os.path.dirname(os.path.dirname(f))).split("__")[0]
        r = replay(f, args.overhead_before - args.overhead_after)
        if not r["requests"]:
            continue
        o = oc.get(task, {})
        saved = 1 - r["estimated_input"] / r["recorded_input"]
        print(f"{task:22s} {r['requests']:4d} {r['recorded_input']:10,d} {r['estimated_input']:10,d} {saved:6.1%} {r['dropped_requests']:4d} {r['overhead_saving_per_request']:7,d}  "
              f"{o.get('input', 0):10,d} {o.get('requests', 0):6d}")
        if r["dropped_reasons"]:
            print(f"{'':22s} dropped: {r['dropped_reasons']}")
        if r["result_cap_tokens_saved_per_later_request"]:
            print(f"{'':22s} result caps: ~{r['result_cap_tokens_saved_per_later_request']:,d} tokens off later requests")


if __name__ == "__main__":
    main()
