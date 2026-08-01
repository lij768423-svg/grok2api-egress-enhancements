package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAllowedGrokManagementPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "/nodes", want: "/api/admin/v1/egress-nodes", ok: true},
		{input: "/nodes/batch", want: "/api/admin/v1/egress-nodes/batch", ok: true},
		{input: "/nodes/9/test", want: "/api/admin/v1/egress-nodes/9/test", ok: true},
		{input: "/nodes/9/quality-test", want: "/api/admin/v1/egress-nodes/9/quality-test", ok: true},
		{input: "/quality-guard/nodes/9/test", want: "/api/admin/v1/egress-quality-guard/nodes/9/test", ok: true},
		{input: "/accounts/export", want: "/api/admin/v1/accounts/export", ok: true},
		{input: "/nodes/9/accounts", want: "/api/admin/v1/egress-nodes/9/accounts", ok: true},
		{input: "/nodes/accounts", want: "/api/admin/v1/egress-nodes/accounts", ok: true},
		{input: "/nodes/../accounts", ok: false},
		{input: "/accounts", want: "/api/admin/v1/accounts", ok: true},
		{input: "/accounts/%2e%2e/settings", ok: false},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, ok := allowedGrokManagementPath(test.input)
			if ok != test.ok || got != test.want {
				t.Fatalf("allowedGrokManagementPath(%q)=(%q,%v), want (%q,%v)", test.input, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestAllowedGrokManagementMethodProtectsCredentialExport(t *testing.T) {
	if allowedGrokManagementMethod(http.MethodGet, "/accounts/export") {
		t.Fatal("GET account export must remain blocked")
	}
	if !allowedGrokManagementMethod(http.MethodPost, "/accounts/export") {
		t.Fatal("selected POST account export must be allowed")
	}
}

func TestRenderStatusPageEmbedsTokens(t *testing.T) {
	page := string(renderStatusPage())
	for _, want := range []string{"Grok2API 管理工作台", "--background:", "data-view=\"accounts\"", "X-Grok2API-Egress-UI", "导出选中"} {
		if !strings.Contains(page, want) {
			t.Fatalf("rendered page missing %q", want)
		}
	}
	if strings.Contains(page, "/*__HALLMARK_TOKENS__*/") {
		t.Fatal("token placeholder was not replaced")
	}
	if strings.Contains(page, "/*__APP_STYLES__*/") || strings.Contains(page, "/*__APP_SCRIPT__*/") {
		t.Fatal("app placeholders were not replaced")
	}
}

func TestComputeTPSUsesGenerationWindow(t *testing.T) {
	if got := computeTPS(1050, 1100, 1000); got != 10500 {
		t.Fatalf("computeTPS()=%v, want 10500", got)
	}
	if got := computeTPS(100, 1000, 1000); got != 0 {
		t.Fatalf("computeTPS()=%v, want 0 for empty generation window", got)
	}
}

func TestUIProxyRejectsMissingSameOriginHeader(t *testing.T) {
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodGet, Path: "/nodes?page=1"})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Body: body}, "/v0/resource/plugins/"+pluginName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"StatusCode":403`) {
		t.Fatalf("response=%s, want 403", raw)
	}
}
