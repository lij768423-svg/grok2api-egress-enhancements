#!/usr/bin/env python3
"""Watch CPA egress-guard plugin + loadtest health while pressure test runs."""

from __future__ import annotations

import json
import os
import time
import urllib.request
from datetime import datetime, timezone
from pathlib import Path


def load_mgmt_key() -> str:
    env = os.environ.get("CPA_MANAGEMENT_KEY", "").strip()
    if env:
        return env
    raise SystemExit("set CPA_MANAGEMENT_KEY before starting the monitor")


def api(mgmt: str, method: str, path: str, timeout: int = 30) -> dict:
    body = json.dumps({"method": method, "path": path}).encode()
    management_base = os.environ.get("CPA_MANAGEMENT_BASE_URL", "http://127.0.0.1:8317").rstrip("/")
    req = urllib.request.Request(
        management_base + "/v0/management/grok2api-egress/api",
        data=body,
        method="POST",
        headers={
            "Authorization": f"Bearer {mgmt}",
            "Content-Type": "application/json",
            "X-Grok2API-Egress-UI": "1",
            "Accept": "application/json",
        },
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def tail_lines(path: Path, n: int = 40) -> list[str]:
    if not path.exists():
        return []
    try:
        data = path.read_text(encoding="utf-8", errors="ignore").splitlines()
        return data[-n:]
    except Exception:
        return []


def main() -> int:
    mgmt = load_mgmt_key()
    log_dir = Path(os.environ.get("CPA_LOADTEST_LOG_DIR", "/var/log/cpa-loadtest"))
    log_dir.mkdir(parents=True, exist_ok=True)
    out = log_dir / "monitor.jsonl"
    interval = float(os.environ.get("MONITOR_INTERVAL", "20"))
    print(f"monitor start -> {out} every {interval}s", flush=True)

    prev_events = 0
    prev_quarantined = 0
    while True:
        ts = datetime.now(tz=timezone.utc).isoformat()
        row: dict = {"ts": ts}
        alerts: list[str] = []
        try:
            status = api(mgmt, "GET", "/quality-guard")
            nodes = api(mgmt, "GET", "/nodes")
            items = nodes.get("items") or (nodes.get("data") or {}).get("items") or []
            node_map = status.get("nodes") or {}
            stats = status.get("statistics") or {}
            events = status.get("recentEvents") or status.get("recent_events") or []

            summary_nodes = []
            healthy = soft = hard = err = quarantined = 0
            for n in items:
                g = node_map.get(str(n.get("id")), {}) or {}
                cls = g.get("last_classification") or ""
                if g.get("disabled_by_guard"):
                    quarantined += 1
                if cls == "healthy":
                    healthy += 1
                elif cls == "soft":
                    soft += 1
                elif cls == "hard":
                    hard += 1
                elif cls == "error":
                    err += 1
                summary_nodes.append(
                    {
                        "id": n.get("id"),
                        "name": n.get("name"),
                        "enabled": n.get("enabled"),
                        "assigned": n.get("assignedAccountCount"),
                        "exitIp": n.get("exitIp"),
                        "cls": cls,
                        "tps": g.get("last_output_tps"),
                        "reason": g.get("last_reason") or "",
                        "quarantined": bool(g.get("disabled_by_guard")),
                        "q_until": g.get("quarantined_until"),
                    }
                )

            row.update(
                {
                    "engine": status.get("engine"),
                    "version": status.get("version"),
                    "available": status.get("available"),
                    "node_count": len(items),
                    "healthy": healthy,
                    "soft": soft,
                    "hard": hard,
                    "error": err,
                    "quarantined": quarantined,
                    "stats": stats,
                    "events_n": len(events),
                    "nodes": summary_nodes,
                    "last_events": events[-5:] if events else [],
                }
            )

            if not status.get("available"):
                alerts.append("plugin_unavailable")
            if status.get("engine") != "cpa-native":
                alerts.append(f"unexpected_engine:{status.get('engine')}")
            if quarantined > prev_quarantined:
                alerts.append(f"new_quarantine count={quarantined}")
            if hard > 0:
                alerts.append(f"hard_class_nodes={hard}")
            if soft > 0:
                alerts.append(f"soft_class_nodes={soft}")
            if err >= 2:
                alerts.append(f"multi_node_errors={err}")
            # cumulative passive hard/soft from plugin statistics
            pas = (stats.get("passive") or {})
            act = (stats.get("active") or {})
            actions = (stats.get("actions") or {})
            ph = int(pas.get("hard") or 0)
            ps = int(pas.get("soft") or 0)
            ah = int(act.get("hard") or 0)
            row["passive_hard"] = ph
            row["passive_soft"] = ps
            row["active_hard"] = ah
            row["actions"] = actions
            # alert when hard hits exist but quarantine never rose
            if (ph + ah) > 0 and int(actions.get("quarantined") or 0) == 0 and quarantined == 0:
                alerts.append(f"ACTION_NEEDED hard_hits={ph+ah} but_quarantine=0")
            if ph > 0 or ah > 0:
                alerts.append(f"stat_hard passive={ph} active={ah}")
            if ps > 0:
                alerts.append(f"stat_soft={ps}")
            # recent unmapped / quarantine events
            for ev in (events[-8:] if events else []):
                et = str(ev.get("event") or "")
                if et.startswith("unmapped_") or et in ("node_quarantined", "quarantine_suppressed"):
                    alerts.append(f"event:{et}:{ev.get('node_name') or ev.get('auth_id') or ''}")
            # imbalance: one channel has 0 assigned while others have many
            assigned = [int(n.get("assigned") or 0) for n in summary_nodes if n.get("enabled")]
            if assigned and max(assigned) > 0 and min(assigned) == 0 and len(assigned) >= 2:
                alerts.append("channel_imbalance_zero_assigned")
            if len(events) > prev_events:
                alerts.append(f"new_events +{len(events) - prev_events}")

            prev_events = len(events)
            prev_quarantined = quarantined
        except Exception as e:
            row["plugin_error"] = str(e)
            alerts.append(f"plugin_api_error:{e}")

        # loadtest live stats (latest jsonl)
        try:
            stats_files = sorted(log_dir.glob("stats-*.jsonl"))
            if stats_files:
                last = tail_lines(stats_files[-1], 1)
                if last:
                    row["loadtest"] = json.loads(last[-1])
                    lt = row["loadtest"]
                    if lt.get("success_rate", 100) < 85 and lt.get("total", 0) >= 20:
                        alerts.append(f"low_success_rate={lt.get('success_rate')}")
                    if (lt.get("errors") or {}).get("http_401", 0) >= 5:
                        alerts.append("many_401")
                    if (lt.get("errors") or {}).get("http_5xx", 0) >= 5:
                        alerts.append("many_5xx")
        except Exception as e:
            row["loadtest_error"] = str(e)

        # container recent errors
        try:
            import subprocess

            p = subprocess.run(
                ["docker", "logs", "cli-proxy-api", "--since", "45s"],
                capture_output=True,
                text=True,
                timeout=10,
            )
            lines = (p.stdout or "") + (p.stderr or "")
            interesting = [
                ln
                for ln in lines.splitlines()
                if any(k in ln.lower() for k in ("panic", "fatal", "egress", "plugin", "error", "401", "429"))
            ]
            row["docker_hits"] = interesting[-8:]
            if any("panic" in ln.lower() for ln in interesting):
                alerts.append("docker_panic")
        except Exception as e:
            row["docker_error"] = str(e)

        row["alerts"] = alerts
        with out.open("a", encoding="utf-8") as f:
            f.write(json.dumps(row, ensure_ascii=False) + "\n")

        # human line
        lt = row.get("loadtest") or {}
        print(
            f"{ts} plugin={row.get('engine')}/{row.get('version')} "
            f"nodes={row.get('node_count')} H/S/Hrd/E/Q="
            f"{row.get('healthy')}/{row.get('soft')}/{row.get('hard')}/{row.get('error')}/{row.get('quarantined')} "
            f"load ok={lt.get('ok')} fail={lt.get('fail')} rate={lt.get('success_rate')} "
            f"alerts={alerts or ['none']}",
            flush=True,
        )
        time.sleep(interval)


if __name__ == "__main__":
    raise SystemExit(main())
