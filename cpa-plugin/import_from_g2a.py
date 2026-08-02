#!/usr/bin/env python3
"""One-shot: export N grok_build accounts from grok2api into CPA auth dir,
bind them round-robin to 3 local egress channels, then DISABLE them on grok2api
so refresh_token is not held on both sides.

Usage (on home-serve):
  python3 import_from_g2a.py --limit 500 \\
    --cpa-dir /data/cliproxyapi/auths \\
    --channels http://127.0.0.1:7951,http://127.0.0.1:7952,http://127.0.0.1:7953
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

CLIENT_ID = "b1a00492-073a-47ea-816f-4c329264a828"
DEFAULT_BASE_URL = "https://cli-chat-proxy.grok.com/v1"
DEFAULT_TOKEN_ENDPOINT = "https://auth.x.ai/oauth2/token"


def load_admin():
    user = os.environ.get("GROK2API_ADMIN_USERNAME", "").strip()
    pw = os.environ.get("GROK2API_ADMIN_PASSWORD", "")
    if not user or not pw:
        raise SystemExit("set GROK2API_ADMIN_USERNAME and GROK2API_ADMIN_PASSWORD for one-shot import")
    return user, pw


def http_json(method: str, url: str, token: str | None = None, body: dict | None = None, timeout: int = 60):
    data = None if body is None else json.dumps(body).encode()
    headers = {"Accept": "application/json", "Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            if not raw:
                return resp.status, {}
            return resp.status, json.loads(raw.decode())
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            payload = json.loads(raw.decode()) if raw else {}
        except Exception:
            payload = {"raw": raw[:300].decode(errors="replace")}
        raise RuntimeError(f"HTTP {e.code} {url}: {payload}") from e


def login(base: str) -> str:
    user, pw = load_admin()
    _, data = http_json("POST", base + "/api/admin/v1/auth/login", body={"username": user, "password": pw})
    return data["data"]["tokens"]["accessToken"]


def list_account_ids(base: str, token: str, limit: int) -> list[str]:
    ids: list[str] = []
    page = 1
    while len(ids) < limit:
        _, data = http_json(
            "GET",
            f"{base}/api/admin/v1/accounts?page={page}&pageSize=100&provider=grok_build",
            token=token,
        )
        items = (data.get("data") or data).get("items") or []
        if not items:
            break
        for item in items:
            status = str(item.get("authStatus") or "").lower()
            if item.get("enabled") is False:
                continue
            if status and status not in ("active", "auth_status_active", ""):
                # skip explicitly bad states when present
                if any(x in status for x in ("reauth", "disabled", "error", "banned", "invalid")):
                    continue
            ids.append(str(item["id"]))
            if len(ids) >= limit:
                break
        page += 1
        if page > 80:
            break
    return ids[:limit]


def export_accounts(base: str, token: str, ids: list[str]) -> list[dict]:
    out: list[dict] = []
    batch = 50
    for i in range(0, len(ids), batch):
        chunk = ids[i : i + batch]
        _, data = http_json(
            "POST",
            base + "/api/admin/v1/accounts/export",
            token=token,
            body={"ids": chunk, "provider": "grok_build"},
            timeout=120,
        )
        accounts = data.get("accounts") or data.get("data") or data
        if isinstance(accounts, dict):
            accounts = accounts.get("accounts") or []
        out.extend(accounts)
        print(f"exported {len(out)}/{len(ids)}", flush=True)
        time.sleep(0.2)
    return out


def disable_on_g2a(base: str, token: str, ids: list[str]) -> None:
    batch = 50
    for i in range(0, len(ids), batch):
        chunk = ids[i : i + batch]
        try:
            http_json(
                "PATCH",
                base + "/api/admin/v1/accounts/batch",
                token=token,
                body={"ids": chunk, "provider": "grok_build", "enabled": False},
                timeout=60,
            )
        except Exception as e:
            print("disable batch warn:", e, flush=True)
        print(f"disabled on g2a {min(i+batch, len(ids))}/{len(ids)}", flush=True)


def jwt_sub_exp(access_token: str) -> tuple[str, str, int]:
    parts = access_token.split(".")
    if len(parts) < 2:
        return "", "", 0
    pad = "=" * (-len(parts[1]) % 4)
    pl = json.loads(base64.urlsafe_b64decode(parts[1] + pad))
    exp = int(pl.get("exp") or 0)
    iat = int(pl.get("iat") or (exp - 21600))
    sub = str(pl.get("sub") or pl.get("principal_id") or "").strip()
    expired = datetime.fromtimestamp(exp, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ") if exp else ""
    return sub, expired, max(exp - iat, 0)


def sanitize(value: str) -> str:
    out = []
    for ch in (value or "").strip():
        if ch.isalnum() or ch in {"@", ".", "_", "-"}:
            out.append(ch)
        else:
            out.append("-")
    return "".join(out).strip("-")


def to_cpa_auth(acc: dict, proxy_url: str) -> dict:
    email = (acc.get("email") or acc.get("name") or "").strip()
    access = acc.get("access_token") or ""
    refresh = acc.get("refresh_token") or ""
    sub, expired, expires_in = jwt_sub_exp(access)
    if not sub:
        sub = str(acc.get("sub") or acc.get("user_id") or acc.get("principal_id") or "").strip()
    if not expired and acc.get("expires_at"):
        expired = str(acc["expires_at"])
        if expired.endswith("+00:00"):
            expired = expired.replace("+00:00", "Z")
    payload = {
        "type": "xai",
        "auth_kind": "oauth",
        "email": email,
        "sub": sub,
        "access_token": access,
        "refresh_token": refresh,
        "id_token": acc.get("id_token") or "",
        "token_type": acc.get("token_type") or "Bearer",
        "expires_in": int(acc.get("expires_in") or expires_in or 21600),
        "expired": expired,
        "last_refresh": datetime.now(tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "token_endpoint": DEFAULT_TOKEN_ENDPOINT,
        "base_url": DEFAULT_BASE_URL,
        "proxy_url": proxy_url,
        "disabled": False,
        "headers": {
            "X-XAI-Token-Auth": "xai-grok-cli",
            "x-grok-client-version": "0.2.93",
            "x-grok-client-identifier": "grok-shell",
        },
        "_imported_from": "g2a-one-shot",
        "_egress_channel": proxy_url,
    }
    return payload


def atomic_write(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = json.dumps(payload, indent=2, ensure_ascii=False) + "\n"
    fd, tmp = tempfile.mkstemp(prefix=".xai-", suffix=".tmp", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
        os.chmod(path, 0o600)
    finally:
        if os.path.exists(tmp):
            try:
                os.unlink(tmp)
            except OSError:
                pass


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default=os.environ.get("GROK2API_BASE_URL", "http://127.0.0.1:8181"))
    ap.add_argument("--limit", type=int, default=500)
    ap.add_argument("--cpa-dir", default=os.environ.get("CPA_AUTH_DIR", "~/.cli-proxy-api"))
    ap.add_argument(
        "--channels",
        default="http://127.0.0.1:7951,http://127.0.0.1:7952,http://127.0.0.1:7953",
    )
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--skip-disable", action="store_true", help="do not disable on g2a (NOT recommended)")
    args = ap.parse_args()
    channels = [c.strip() for c in args.channels.split(",") if c.strip()]
    if len(channels) < 1:
        raise SystemExit("need at least one channel")

    print("login g2a (import only)...", flush=True)
    token = login(args.base)
    print("list accounts...", flush=True)
    ids = list_account_ids(args.base, token, args.limit)
    print(f"selected {len(ids)} accounts", flush=True)
    if not ids:
        return 1
    accounts = export_accounts(args.base, token, ids)
    print(f"got {len(accounts)} credential blobs", flush=True)

    cpa_dir = Path(args.cpa_dir).expanduser()
    written = 0
    for i, acc in enumerate(accounts):
        email = (acc.get("email") or acc.get("name") or f"unknown-{i}").strip()
        if not acc.get("refresh_token") or not acc.get("access_token"):
            print("skip missing tokens", email, flush=True)
            continue
        proxy = channels[i % len(channels)]
        payload = to_cpa_auth(acc, proxy)
        fname = f"xai-{sanitize(email)}.json"
        dest = cpa_dir / fname
        if args.dry_run:
            print("DRY", dest, "->", proxy)
        else:
            atomic_write(dest, payload)
        written += 1
    print(f"wrote {written} CPA auth files into {cpa_dir}", flush=True)

    if args.dry_run or args.skip_disable:
        print("skip g2a disable", flush=True)
    else:
        print("disable exported accounts on g2a (token exclusivity)...", flush=True)
        disable_on_g2a(args.base, token, ids)
        print("done: refresh_token now only on CPA side for imported set", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
