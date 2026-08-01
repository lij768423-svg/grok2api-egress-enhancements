# grok2api egress enhancement patches

This is an unofficial patch distribution for [chenyme/grok2api](https://github.com/chenyme/grok2api). It adds immediate fixed-proxy recovery and an optional egress quality guard without copying the complete upstream repository.

Current baseline:

- Upstream release: `v3.0.11`
- Upstream commit: `090104504b403d65675a01dab9c92b3a235ee832`
- Patch commit: `690a641deb06c5c1a73983677f6b454557727113`
- Follow-up fix patch: `patches/0002-fix-quality-guard-empty-node-selection.patch`
- Upstream draft PR: [chenyme/grok2api#837](https://github.com/chenyme/grok2api/pull/837)
- Runnable fork: [lij768423-svg/grok2api](https://github.com/lij768423-svg/grok2api/tree/main)

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
- An empty `QUALITY_GUARD_NODE_IDS` monitors all enabled `grok_build` nodes with configured proxies.
- Admin UI, manual diagnostics, hot-reloadable policy, and persistent statistics.
- Python sidecar, Docker Compose and systemd examples, security notes, and bilingual documentation.

The quality guard is a heuristic circuit breaker, not proof that upstream model capability changed. Immediate hard quarantine is intentionally aggressive; raise `hard_tps` when false positives are more costly. Soft anomalies still require confirmation probes.


## Apply directly

From a clean grok2api checkout:

```sh
git fetch --tags origin
git checkout -b egress-enhancements v3.0.11
git am --3way \
  /path/to/grok2api-egress-enhancements/patches/0001-feat-add-egress-recovery-and-quality-guard.patch \
  /path/to/grok2api-egress-enhancements/patches/0002-fix-quality-guard-empty-node-selection.patch
```

For newer upstream versions, follow [AI_MERGE_GUIDE.md](./docs/AI_MERGE_GUIDE.md) and resolve conflicts according to the documented invariants instead of replacing newer files wholesale.

## Validate

```sh
go test ./...
python3 -m unittest -v tools/egress-quality-guard/quality_guard_test.py
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
