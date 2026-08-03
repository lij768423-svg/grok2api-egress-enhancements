package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputeTPSUsesGenerationWindow(t *testing.T) {
	// 1050 tokens over 500ms generation window (1100-600) => 2100 TPS
	if got := computeTPS(1050, 1100, 600); got < 2099 || got > 2101 {
		t.Fatalf("computeTPS()=%v, want ~2100", got)
	}
	// tiny generation window falls back to full duration (avoid false hard)
	// 100 tokens / 1000ms => 100 TPS
	if got := computeTPS(100, 1000, 950); got < 99 || got > 101 {
		t.Fatalf("computeTPS()=%v, want ~100 with min window fallback", got)
	}
	if got := computeTPS(100, 0, 0); got != 0 {
		t.Fatalf("computeTPS()=%v, want 0", got)
	}
}

func TestClassifyTPS(t *testing.T) {
	if classifyTPS(1200, 500, 1000) != "hard" {
		t.Fatal("expected hard")
	}
	if classifyTPS(600, 500, 1000) != "soft" {
		t.Fatal("expected soft")
	}
	if classifyTPS(100, 500, 1000) != "healthy" {
		t.Fatal("expected healthy")
	}
}

func TestStoreNodeCRUD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := newStateStore(path)
	n, err := s.createNode("ch-1", "http://127.0.0.1:7951", true, false, 200)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID == "" || n.ProxyURL == "" {
		t.Fatalf("bad node %#v", n)
	}
	pub := publicNode(n)
	if _, ok := pub["proxy_url"]; ok {
		t.Fatal("public node must not expose proxy_url")
	}
	if pub["hasProxy"] != true {
		t.Fatal("hasProxy")
	}
	list := s.listNodes()
	if len(list) != 1 {
		t.Fatalf("len=%d", len(list))
	}
	// reload
	s2 := newStateStore(path)
	n2, ok := s2.getNode(n.ID)
	if !ok || n2.ProxyURL != "http://127.0.0.1:7951" {
		t.Fatalf("reload failed %#v", n2)
	}
	_ = s2.deleteNodes([]string{n.ID})
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCreateNodesIsAllOrNothing(t *testing.T) {
	s := newStateStore(filepath.Join(t.TempDir(), "state.json"))
	created, err := s.createNodes([]nodeCreateInput{
		{Name: "a", ProxyURL: "http://127.0.0.1:7951", Enabled: true, AccountCapacity: 100},
		{Name: "b", ProxyURL: "http://127.0.0.1:7952", Enabled: true, ProxyPool: true, AccountCapacity: 120},
	})
	if err != nil || len(created) != 2 || len(s.listNodes()) != 2 {
		t.Fatalf("created=%d nodes=%d err=%v", len(created), len(s.listNodes()), err)
	}
	if _, err := s.createNodes([]nodeCreateInput{
		{Name: "valid", ProxyURL: "http://127.0.0.1:7953", Enabled: true},
		{Name: "invalid", ProxyURL: "", Enabled: true},
	}); err == nil {
		t.Fatal("expected invalid import to fail")
	}
	if len(s.listNodes()) != 2 {
		t.Fatal("invalid batch must not create partial nodes")
	}
}

func TestRenderStatusPage(t *testing.T) {
	page := strings.Replace(pageTemplate, "/*__HALLMARK_TOKENS__*/", tokenCSS, 1)
	for _, want := range []string{"出口守护", "纯 CPA", "data-batch=\"enable\"", "重平衡账号", "批量添加", "/nodes/import", "页面每 15 秒刷新", "X-Grok2API-Egress-UI"} {
		if !strings.Contains(page, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(page, "/*__HALLMARK_TOKENS__*/") {
		t.Fatal("tokens not replaced in test helper path only")
	}
}

func TestUIProxyRejectsMissingHeader(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodGet, Path: "/nodes"})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"StatusCode":403`) {
		t.Fatalf("got %s", raw)
	}
}

func TestDispatchNodesList(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	_, _ = store.createNode("a", "http://127.0.0.1:1", true, false, 0)
	headers := make(http.Header)
	headers.Set("X-Grok2API-Egress-UI", "1")
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodGet, Path: "/nodes"})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Headers: headers, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var resp managementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Body)
	}
	if !strings.Contains(string(resp.Body), `"name":"a"`) {
		t.Fatalf("body %s", resp.Body)
	}
}

func TestDispatchNodesImportRedactsProxyURLs(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	headers := make(http.Header)
	headers.Set("X-Grok2API-Egress-UI", "1")
	requestBody, _ := json.Marshal(map[string]any{
		"items": []map[string]any{
			{"name": "fixed-a", "proxyURL": "http://user:pass@127.0.0.1:7951", "accountCapacity": 100},
			{"proxy_url": "http://user:pass@127.0.0.1:7952", "proxy_pool": true},
		},
	})
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodPost, Path: "/nodes/import", Body: requestBody})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Headers: headers, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var resp managementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"created":2`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), "user:pass") || strings.Contains(string(resp.Body), "proxy_url") {
		t.Fatalf("response leaked proxy URL: %s", resp.Body)
	}
	if len(store.listNodes()) != 2 {
		t.Fatalf("node count=%d", len(store.listNodes()))
	}
}
