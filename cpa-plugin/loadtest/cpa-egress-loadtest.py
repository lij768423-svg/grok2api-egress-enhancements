#!/usr/bin/env python3
"""CPA egress loadtest — continuous concurrent chat against home CPA.

Designed to stress the pure-CPA egress guard (3 channels + quality isolation).

Examples:
  python3 cpa-egress-loadtest.py --workers 8 --max-tokens 256
  python3 cpa-egress-loadtest.py --workers 16 --hours 2 --stream
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import sys
import threading
import time
import urllib.error
import urllib.request
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from pathlib import Path

# Never send loadtest traffic through mihomo/system HTTP_PROXY — hit CPA directly.
for _k in ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "all_proxy"):
    os.environ.pop(_k, None)
os.environ.setdefault("NO_PROXY", "*")
os.environ.setdefault("no_proxy", "*")

# Force urllib to ignore residual proxy env
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
urllib.request.install_opener(_OPENER)

PROMPTS = [
    "Reply with exactly OK.",
    "Return only the single word PONG.",
    "Say hi in three words max.",
    "Output only: loadtest-ok",
    "Reply with a one-word status.",
]


class Stats:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.ok = 0
        self.fail = 0
        self.bytes_out = 0
        self.tokens_out = 0
        self.lat_ms: list[int] = []
        self.errors: Counter[str] = Counter()
        self.started = time.time()
        self.stop = False

    def add_ok(self, ms: int, tokens: int, nbytes: int) -> None:
        with self.lock:
            self.ok += 1
            self.tokens_out += tokens
            self.bytes_out += nbytes
            self.lat_ms.append(ms)
            if len(self.lat_ms) > 5000:
                self.lat_ms = self.lat_ms[-2500:]

    def add_fail(self, kind: str) -> None:
        with self.lock:
            self.fail += 1
            self.errors[kind] += 1

    def snapshot(self) -> dict:
        with self.lock:
            lats = sorted(self.lat_ms)
            def pct(p: float) -> int:
                if not lats:
                    return 0
                return lats[min(len(lats) - 1, int(len(lats) * p))]
            elapsed = max(0.001, time.time() - self.started)
            total = self.ok + self.fail
            return {
                "elapsed_sec": round(elapsed, 1),
                "ok": self.ok,
                "fail": self.fail,
                "total": total,
                "rps": round(total / elapsed, 2),
                "ok_rps": round(self.ok / elapsed, 2),
                "success_rate": round(100.0 * self.ok / total, 2) if total else 0,
                "tokens_out": self.tokens_out,
                "tps_approx": round(self.tokens_out / elapsed, 1),
                "p50_ms": pct(0.50),
                "p95_ms": pct(0.95),
                "p99_ms": pct(0.99),
                "errors": dict(self.errors.most_common(8)),
            }


def classify_error(exc: BaseException, body: str = "") -> str:
    text = f"{exc} {body}".lower()
    if "401" in text or "unauthorized" in text:
        return "http_401"
    if "403" in text or "forbidden" in text:
        return "http_403"
    if "429" in text or "rate" in text:
        return "http_429"
    if "500" in text or "502" in text or "503" in text or "504" in text:
        return "http_5xx"
    if "timeout" in text or "timed out" in text:
        return "timeout"
    if "connection" in text or "reset" in text or "refused" in text:
        return "connection"
    return "other"


def one_request(args: argparse.Namespace, stats: Stats, worker_id: int, seq: int) -> None:
    prompt = PROMPTS[seq % len(PROMPTS)]
    payload = {
        "model": args.model,
        "messages": [{"role": "user", "content": prompt}],
        "stream": bool(args.stream),
        "max_tokens": args.max_tokens,
        "temperature": 0.7,
    }
    data = json.dumps(payload).encode()
    req = urllib.request.Request(
        args.base.rstrip("/") + "/chat/completions",
        data=data,
        method="POST",
        headers={
            "Authorization": f"Bearer {args.api_key}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream" if args.stream else "application/json",
            "User-Agent": f"cpa-egress-loadtest/1.0 w{worker_id}",
        },
    )
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=args.timeout) as resp:
            raw = resp.read()
            ms = int((time.time() - t0) * 1000)
            tokens = 0
            if args.stream:
                # count rough output from SSE data lines
                for line in raw.decode("utf-8", errors="ignore").splitlines():
                    if not line.startswith("data:"):
                        continue
                    chunk = line[5:].strip()
                    if not chunk or chunk == "[DONE]":
                        continue
                    try:
                        obj = json.loads(chunk)
                    except json.JSONDecodeError:
                        continue
                    u = obj.get("usage") or {}
                    tokens = max(
                        tokens,
                        int(u.get("completion_tokens") or 0)
                        + int(u.get("output_tokens") or 0)
                        + int(u.get("reasoning_tokens") or 0),
                    )
                    for ch in obj.get("choices") or []:
                        delta = (ch.get("delta") or {})
                        c = delta.get("content") or ""
                        if c and tokens == 0:
                            tokens += max(1, len(c) // 4)
            else:
                obj = json.loads(raw.decode("utf-8", errors="ignore") or "{}")
                if obj.get("error"):
                    stats.add_fail(classify_error(RuntimeError(str(obj["error"])), str(obj)))
                    return
                u = obj.get("usage") or {}
                tokens = int(u.get("completion_tokens") or 0) + int(u.get("reasoning_tokens") or 0)
            stats.add_ok(ms, tokens, len(raw))
    except Exception as e:
        body = ""
        if isinstance(e, urllib.error.HTTPError):
            try:
                body = e.read().decode("utf-8", errors="ignore")[:300]
            except Exception:
                body = ""
        stats.add_fail(classify_error(e, body))


def worker_loop(args: argparse.Namespace, stats: Stats, worker_id: int) -> None:
    seq = 0
    while not stats.stop:
        one_request(args, stats, worker_id, seq)
        seq += 1
        if args.sleep > 0:
            time.sleep(args.sleep)


def main() -> int:
    ap = argparse.ArgumentParser(description="CPA egress continuous loadtest")
    ap.add_argument("--base", default=os.environ.get("CPA_URL", "http://127.0.0.1:8317/v1"))
    ap.add_argument("--api-key", default=os.environ.get("CPA_LOADTEST_API_KEY") or os.environ.get("CPA_KEY", ""))
    ap.add_argument("--model", default="grok-4.5")
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--max-tokens", type=int, default=256)
    ap.add_argument("--sleep", type=float, default=0.3, help="per-worker pause after each request")
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--hours", type=float, default=0, help="0 = run until signal")
    ap.add_argument("--stream", action="store_true")
    ap.add_argument("--report-every", type=float, default=15.0)
    ap.add_argument("--log-dir", default="/data/cliproxyapi/logs/loadtest")
    args = ap.parse_args()

    if not args.api_key:
        print("missing --api-key / CPA_LOADTEST_API_KEY", file=sys.stderr)
        return 2

    log_dir = Path(args.log_dir)
    log_dir.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(tz=timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    stats_path = log_dir / f"stats-{stamp}.jsonl"
    run_path = log_dir / f"run-{stamp}.log"

    stats = Stats()

    def log(msg: str) -> None:
        line = f"{datetime.now(tz=timezone.utc).isoformat()} {msg}"
        print(line, flush=True)
        with run_path.open("a", encoding="utf-8") as f:
            f.write(line + "\n")

    def handle_sig(signum, frame) -> None:  # noqa: ARG001
        log(f"signal {signum}, stopping…")
        stats.stop = True

    signal.signal(signal.SIGINT, handle_sig)
    signal.signal(signal.SIGTERM, handle_sig)

    deadline = time.time() + args.hours * 3600 if args.hours > 0 else 0
    log(
        f"start base={args.base} model={args.model} workers={args.workers} "
        f"max_tokens={args.max_tokens} sleep={args.sleep} stream={args.stream} "
        f"hours={args.hours or 'inf'} stats={stats_path}"
    )

    with ThreadPoolExecutor(max_workers=args.workers) as ex:
        for i in range(args.workers):
            ex.submit(worker_loop, args, stats, i)

        while not stats.stop:
            time.sleep(args.report_every)
            snap = stats.snapshot()
            with stats_path.open("a", encoding="utf-8") as f:
                f.write(json.dumps({"ts": time.time(), **snap}, ensure_ascii=False) + "\n")
            log(
                f"ok={snap['ok']} fail={snap['fail']} rate={snap['success_rate']}% "
                f"rps={snap['rps']} tok/s~{snap['tps_approx']} "
                f"p50={snap['p50_ms']}ms p95={snap['p95_ms']}ms errors={snap['errors']}"
            )
            if deadline and time.time() >= deadline:
                log("hours elapsed, stopping…")
                stats.stop = True
                break

    final = stats.snapshot()
    final_path = log_dir / f"final-{stamp}.json"
    final_path.write_text(json.dumps(final, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    log(f"done final={final_path} {json.dumps(final, ensure_ascii=False)}")
    return 0 if final["fail"] == 0 or final["ok"] > 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
