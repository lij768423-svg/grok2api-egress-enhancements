# CPA Grok2API Egress plugin

This optional CLIProxyAPI plugin embeds a Grok2API administration workbench in
CLIProxyAPI. Version 0.4.0 provides:

- dashboard usage and resource summaries;
- account search, provider/status filters, pagination, editing, batch state,
  quota/token refresh, egress assignment, deletion, and selected export;
- model route list/create/edit/delete, sync, and batch state operations;
- Client Key list/create/edit/delete, secret reveal, and batch state operations;
- request audit filters, cursor pagination, and full attempt details;
- complete revision-protected Grok2API settings editing;
- live guard status, statistics, and light/dark themes;
- node add, edit, delete, enable, disable, search, and batch operations;
- connectivity checks and real-model quality tests;
- editable active/passive/hybrid guard policy;
- server-side grok2api administrator login, so proxy URLs and administrator
  credentials are never returned to the browser.

The egress view intentionally manages only `grok_build` nodes. Saved proxy URLs
are write-only: editing with an empty proxy field preserves the existing value.

Account credential export is deliberately narrower than upstream's full export:

- only `POST /accounts/export` is exposed by the plugin bridge;
- the user must explicitly select accounts from one provider and confirm the
  sensitive download;
- full-pool `GET /accounts/export` is rejected with HTTP 405;
- downloads retain `Cache-Control: no-store` and are not written to plugin logs.

## Build

Requirements: Go 1.26+, CGO, and a C compiler.

```sh
cd cpa-plugin/go
go test ./...
go build -buildmode=c-shared -trimpath -o grok2api-egress.so .
```

Copy the resulting `.so` into CLIProxyAPI's plugin directory and enable it:

```yaml
plugins:
  enabled: true
  configs:
    grok2api-egress:
      enabled: true
      priority: 2
      grok2api_base_url: "http://100.102.32.24:8181"
      hard_tps: 1000
      soft_tps: 500
      disable_on_hard: false
      fetch_timeout_sec: 4
```

Set `GROK2API_ADMIN_USERNAME` and `GROK2API_ADMIN_PASSWORD` only in the
CLIProxyAPI process environment. Restart CLIProxyAPI, sign in to its management
panel, then open **Grok2API Egress**. Mutations use CLIProxyAPI's authenticated
Management API; the grok2api access token is never exposed to the browser.

The direct plugin page is registered at:

```text
/v0/resource/plugins/grok2api-egress/status
```
