# Grok2API & CPA egress quality guard

This is an unofficial enhancement distribution for [chenyme/grok2api](https://github.com/chenyme/grok2api): it provides immediate fixed-proxy recovery and egress quality-guard patches, plus a pure CPA-native plugin with no Grok2API runtime dependency. The repository does not copy the complete upstream source.

Current baseline:

- Upstream release: `v3.0.11`
- Upstream commit: `090104504b403d65675a01dab9c92b3a235ee832`
- Patch commit: `334dbe0f01ea0294318856873136c4196f835a04`
- Upstream draft PR: [chenyme/grok2api#837](https://github.com/chenyme/grok2api/pull/837)
- Runnable fork: [lij768423-svg/grok2api](https://github.com/lij768423-svg/grok2api/tree/agent/egress-resilience-quality-guard)

## Features

### Immediate fixed-proxy recovery

- A pre-submission transport failure persists cooldown state and starts an immediate background probe.
- Concurrent failures for one node share a single probe.
- A later bound request waits for at most five seconds, reloads persisted state after healthy recovery, and continues early.
- Request cancellation stops the wait without canceling the shared probe.
- Submitted generation requests, authentication failures, quota exhaustion, and rate limits are never replayed by this mechanism.
- Upstream's existing proxy-pool mode keeps fresh-tunnel isolation, so one rotating exit failure does not cool the whole pool.

### Egress quality guard

- Passive audits use the grok2api panel formula `output tokens / (duration - first token)`; output tokens include reasoning tokens.
- **Passive hard-threshold hits quarantine immediately**. Soft hits still trigger a fixed-prompt active confirmation and require consecutive strikes.
- Active soft and hard thresholds, consecutive probe-error handling, minimum healthy-node protection, quarantine, and recovery.
- Fail-closed quarantine before confirmation, with same-IP confirmation for short buffered bursts to avoid false rotation.
- A trusted per-node rotation webhook and a 1024Proxy `sid-...-t-...` sticky-session rotator.
- One real-model check per new IP: healthy results restore immediately; anomalous or indeterminate results remain isolated.
- Account-selection failures are deferred without counting a proxy error or rotating the IP.
- If a target node's bound accounts are unavailable, administrator probes borrow any healthy Build account while still forcing the physical request through the node under test. Ordinary traffic is unchanged.
- If the entire account pool is unavailable, the guard uses a separate long backoff and suppresses duplicate no-account logs while keeping the node isolated.
- Admin UI, manual diagnostics, hot-reloadable policy, and persistent statistics.
- One replaceable toast per node action, with manual tests disabled while a node is quarantined or rotating.
- Create, edit, delete, enable, disable, and refresh Build proxy nodes directly from the node-quality table.
- Select individual or all nodes and batch enable, disable, or delete them with destructive-action confirmation.
- Automatically discover proxied Build nodes when `QUALITY_GUARD_NODE_IDS` is empty while publishing resolved IDs for compatibility with older admin pages.
- Python sidecar, Docker Compose and systemd examples, security notes, and bilingual documentation.

### CPA-native egress guard plugin

`cpa-plugin/` is now the **v1.0.3 pure-CPA plugin**. It has no runtime dependency on Grok2API: it uses CPA Host APIs for auth files and usage events, binds `proxy_url` stickily to egress nodes, and provides node CRUD, batch operations, connectivity/real-model tests, quarantine migration, hot-reloadable policy, statistics, events, and light/dark themes. See [cpa-plugin/README.md](./cpa-plugin/README.md) for build instructions and the Chinese [AI deployment and operations guide](./cpa-plugin/AI_USAGE_GUIDE.md) for proxy topology, capacity planning, quarantine recovery, and forced residential-IP rotation.

The quality guard is a heuristic circuit breaker, not proof that upstream model capability changed. Immediate hard quarantine is intentionally aggressive; raise `hard_tps` when false positives are more costly. Soft anomalies still require confirmation probes.


## Apply directly

From a clean grok2api checkout:

```sh
git fetch --tags origin
git checkout -b egress-enhancements v3.0.11
git am --3way /path/to/grok2api-egress-enhancements/patches/0001-feat-add-egress-recovery-and-quality-guard.patch
```

For newer upstream versions, follow [AI_MERGE_GUIDE.md](./docs/AI_MERGE_GUIDE.md) and resolve conflicts according to the documented invariants instead of replacing newer files wholesale.

## Validate

```sh
go test ./...
python3 -m unittest -v \
  tools/egress-quality-guard/quality_guard_test.py \
  tools/egress-quality-guard/session_rotator_test.py  # 26 tests
cd frontend
pnpm lint
pnpm build
```

## Security and privacy

Never provide real environment files, application config, databases, state volumes, proxy URLs, account credentials, or production logs to an AI merge tool. The upstream source, this patch, and sanitized test failures are sufficient.

## Related projects

- [Grok Register + Live Panel](https://github.com/lij768423-svg/grok-register-panel): a separate Camoufox-based Grok registration workflow and web control panel with multiple email backends, an external proxy pool, egress checks, an ASN blacklist, runtime statistics, and account recovery. It is not bundled with this patch.

## Friends

- [LINUX DO](https://linux.do) — A new kind of community

## License and attribution

The patch is distributed under the upstream MIT license. Preserve the upstream LICENSE, copyright notices, and Git history. This repository is not an official grok2api release and does not imply upstream endorsement.
