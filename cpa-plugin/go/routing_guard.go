package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var schedulerCursor atomic.Uint64

func isXAIProvider(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return strings.Contains(provider, "xai") || strings.Contains(provider, "grok")
}

func requestIncludesXAI(provider string, providers []string) bool {
	if isXAIProvider(provider) {
		return true
	}
	for _, candidate := range providers {
		if isXAIProvider(candidate) {
			return true
		}
	}
	return false
}

func schedulerCandidateAvailable(candidate pluginapi.SchedulerAuthCandidate) bool {
	status := strings.ToLower(strings.TrimSpace(candidate.Status))
	// Empty status is retained for older CPA hosts. Any explicit lifecycle or
	// cooldown state other than active/ready must stay with CPA's retry logic.
	switch status {
	case "", "active", "ready":
		return true
	case "disabled", "unavailable", "error", "cooling", "cooldown", "pending", "refreshing", "retrying":
		return false
	default:
		// Unknown explicit states are fail-closed; selecting them can bypass CPA's
		// own cooldown scheduler.
		return false
	}
}

// handleSchedulerPick keeps the host scheduler from selecting an auth whose
// proxy node is quarantined. Unmanaged providers and unbound accounts are
// delegated to CPA's native scheduler.
func handleSchedulerPick(request []byte) ([]byte, error) {
	ensureStore()
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode scheduler request: %w", err)
	}
	if !requestIncludesXAI(req.Provider, req.Providers) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	nodes := store.listNodes()
	byProxy := make(map[string]*nodeRecord, len(nodes))
	for _, node := range nodes {
		if node.ProxyURL != "" {
			byProxy[node.ProxyURL] = node
		}
	}
	cache := refreshAuthProxyCache()
	eligible := make([]string, 0, len(req.Candidates))
	managed := false
	nonXAIAvailable := false
	for _, candidate := range req.Candidates {
		if !isXAIProvider(candidate.Provider) {
			nonXAIAvailable = true
			continue
		}
		proxy := cache[candidate.ID]
		if proxy == "" {
			continue
		}
		node := byProxy[proxy]
		if node == nil {
			continue
		}
		managed = true
		if !schedulerCandidateAvailable(candidate) {
			continue
		}
		if node.Enabled && !node.DisabledByGuard {
			eligible = append(eligible, candidate.ID)
		}
	}
	if !managed {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	if len(eligible) == 0 {
		if strings.TrimSpace(req.Provider) == "" && nonXAIAvailable {
			return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
		}
		return errorEnvelope("egress_no_healthy_auth", "没有可用的健康 CPA 出口账号"), nil
	}
	index := schedulerCursor.Add(1) - 1
	selected := eligible[index%uint64(len(eligible))]
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected})
}

// handleRequestInterceptAfterAuth closes the small race between auth selection
// and synchronous quarantine migration. A request selected during that window
// receives a retryable response instead of reaching a known bad egress.
func handleRequestIntercept(request []byte, afterAuth bool) ([]byte, error) {
	ensureStore()
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode request interceptor request: %w", err)
	}
	if !afterAuth || len(req.Metadata) == 0 {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	selected := firstString(req.Metadata, "selected_auth_id", "selectedAuthID", "auth_id", "authID")
	if selected == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	nodeID := resolveNodeIDForAuth(store, selected)
	if nodeID == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	node, ok := store.getNode(nodeID)
	if !ok || !node.DisabledByGuard {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "egress_quarantined",
			"message": "当前账号出口正在隔离迁移，请重试",
		},
	})
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Terminate:  true,
		StatusCode: http.StatusServiceUnavailable,
		ResponseHeaders: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"1"},
		},
		ResponseBody: body,
	})
}
