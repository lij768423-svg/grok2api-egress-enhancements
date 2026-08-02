package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func computeTPS(outputTokens, durationMs, firstTokenMs int64) float64 {
	if outputTokens <= 0 || durationMs <= 0 {
		return 0
	}
	denom := durationMs - firstTokenMs
	// Short replies often have firstToken ≈ duration, which blows up TPS and
	// false-triggers hard quarantine. Require a minimum generation window.
	const minGenMs int64 = 200
	if denom < minGenMs {
		denom = durationMs
	}
	if denom < minGenMs {
		return 0
	}
	// Ignore tiny outputs for hard-class decisions upstream; still return TPS.
	return float64(outputTokens) / (float64(denom) / 1000.0)
}

// authProxyCache maps auth id/index/name → proxy_url (refreshed periodically).
var (
	authProxyMu    sync.Mutex
	authProxyCache map[string]string
	authProxyAt    time.Time
)

func refreshAuthProxyCache() map[string]string {
	authProxyMu.Lock()
	defer authProxyMu.Unlock()
	if authProxyCache != nil && time.Since(authProxyAt) < 15*time.Second {
		return authProxyCache
	}
	out := map[string]string{}
	auths, err := listAuthFiles()
	if err == nil {
		for _, a := range auths {
			if a.ProxyURL == "" {
				continue
			}
			if a.Index != "" {
				out[a.Index] = a.ProxyURL
			}
			if a.Name != "" {
				out[a.Name] = a.ProxyURL
				out[strings.TrimSuffix(a.Name, ".json")] = a.ProxyURL
			}
			if a.Email != "" {
				out["xai-"+a.Email+".json"] = a.ProxyURL
				out[a.Email] = a.ProxyURL
			}
			if a.Path != "" {
				out[a.Path] = a.ProxyURL
				out[filepath.Base(a.Path)] = a.ProxyURL
			}
		}
	}
	authProxyCache = out
	authProxyAt = time.Now()
	return out
}

func resolveNodeIDForAuth(store *stateStore, authKeys ...string) string {
	cache := refreshAuthProxyCache()
	var proxy string
	for _, k := range authKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if p, ok := cache[k]; ok && p != "" {
			proxy = p
			break
		}
		// try host get as fallback
		if a, err := getAuthFile(k); err == nil && a.ProxyURL != "" {
			proxy = a.ProxyURL
			break
		}
	}
	if proxy == "" {
		return ""
	}
	for _, n := range store.listNodes() {
		if n.ProxyURL == proxy {
			return n.ID
		}
	}
	return ""
}

func classifyTPS(tps float64, soft, hard float64) string {
	if tps <= 0 {
		return "unknown"
	}
	if tps >= hard {
		return "hard"
	}
	if tps >= soft {
		return "soft"
	}
	return "healthy"
}

func httpClientThroughProxy(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if strings.TrimSpace(proxyURL) != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("代理 URL 无效")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

func probeConnectivity(proxyURL string) (exitIP string, latencyMs int64, err error) {
	client, err := httpClientThroughProxy(proxyURL, 20*time.Second)
	if err != nil {
		return "", 0, err
	}
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "https://api.ipify.org", nil)
	req.Header.Set("User-Agent", "CPA-egress-guard/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Since(start).Milliseconds(), err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ip := strings.TrimSpace(string(body))
	if resp.StatusCode >= 400 || ip == "" {
		return "", time.Since(start).Milliseconds(), fmt.Errorf("连通性失败 HTTP %d", resp.StatusCode)
	}
	return ip, time.Since(start).Milliseconds(), nil
}

type qualityResult struct {
	Classification string  `json:"classification"`
	TPS            float64 `json:"tps"`
	OutputTokens   int64   `json:"output_tokens"`
	DurationMs     int64   `json:"duration_ms"`
	FirstTokenMs   int64   `json:"first_token_ms"`
	ExitIP         string  `json:"exit_ip,omitempty"`
	Error          string  `json:"error,omitempty"`
	Model          string  `json:"model,omitempty"`
}

func applyGrokClientHeaders(req *http.Request, auth authFile) {
	// Always force Grok CLI headers — missing X-XAI-Token-Auth yields
	// upstream 401 "x_xai_token_auth=none / no auth context".
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", "0.2.93")
	req.Header.Set("x-grok-client-identifier", "grok-shell")
	req.Header.Set("User-Agent", "CPA-egress-guard/1.0")
	if headers, ok := auth.Raw["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				req.Header.Set(k, s)
			}
		}
	}
	// Re-assert critical headers after auth map copy (auth may contain empty values).
	if req.Header.Get("X-XAI-Token-Auth") == "" {
		req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	}
	if req.Header.Get("x-grok-client-version") == "" {
		req.Header.Set("x-grok-client-version", "0.2.93")
	}
	if req.Header.Get("x-grok-client-identifier") == "" {
		req.Header.Set("x-grok-client-identifier", "grok-shell")
	}
}

func isAuthExpired(auth authFile) bool {
	exp, _ := auth.Raw["expired"].(string)
	if exp == "" {
		return false
	}
	// accept RFC3339 / RFC3339Nano / trailing Z variants
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, exp); err == nil {
			return time.Now().After(t.Add(-2 * time.Minute))
		}
	}
	// bare "2026-08-02T05:04:09Z" already RFC3339
	if t, err := time.Parse("2006-01-02T15:04:05Z07:00", exp); err == nil {
		return time.Now().After(t.Add(-2 * time.Minute))
	}
	return false
}

func isAuthErrorRetryable(status int, body string) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "invalid or expired") ||
		strings.Contains(lower, "no auth context") ||
		strings.Contains(lower, "permissiondenied") ||
		strings.Contains(lower, "x_xai_token_auth=none")
}

func probeQuality(store *stateStore, node *nodeRecord) qualityResult {
	pol := store.policy()
	res := qualityResult{Model: pol.Model}
	if node == nil || node.ProxyURL == "" {
		res.Classification = "error"
		res.Error = "节点缺少代理"
		return res
	}

	// connectivity first for exit IP
	if ip, _, errIP := probeConnectivity(node.ProxyURL); errIP == nil {
		res.ExitIP = ip
	}

	candidates, err := listAuthsForNode(node, 8)
	if err != nil || len(candidates) == 0 {
		res.Classification = "error"
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Error = "没有可用的 CPA xAI 账号"
		}
		return res
	}

	client, err := httpClientThroughProxy(node.ProxyURL, 90*time.Second)
	if err != nil {
		res.Classification = "error"
		res.Error = err.Error()
		return res
	}

	maxTok := pol.MaxOutputTokensProbe
	if maxTok <= 0 {
		maxTok = 256
	}
	payload := map[string]any{
		"model": pol.Model,
		"messages": []map[string]string{
			{"role": "user", "content": "Write a detailed technical explanation of how TCP slow start works, at least 12 sentences, plain text only."},
		},
		"stream":      true,
		"max_tokens":  maxTok,
		"temperature": 0.7,
	}
	body, _ := json.Marshal(payload)

	var lastErr string
	for i, auth := range candidates {
		token, _ := auth.Raw["access_token"].(string)
		if strings.TrimSpace(token) == "" {
			lastErr = "账号缺少 access_token"
			continue
		}
		if isAuthExpired(auth) && i+1 < len(candidates) {
			// Prefer non-expired accounts first; last candidate still tried.
			continue
		}
		baseURL, _ := auth.Raw["base_url"].(string)
		if baseURL == "" {
			baseURL = "https://cli-chat-proxy.grok.com/v1"
		}
		baseURL = strings.TrimRight(baseURL, "/")

		req, errReq := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
		if errReq != nil {
			res.Classification = "error"
			res.Error = "无法创建探测请求"
			return res
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		applyGrokClientHeaders(req, auth)

		start := time.Now()
		resp, errDo := client.Do(req)
		if errDo != nil {
			lastErr = "模型探测请求失败: " + truncate(errDo.Error(), 120)
			res.DurationMs = time.Since(start).Milliseconds()
			continue
		}

		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			msg := fmt.Sprintf("上游 HTTP %d: %s", resp.StatusCode, truncate(string(b), 160))
			lastErr = msg
			res.DurationMs = time.Since(start).Milliseconds()
			if isAuthErrorRetryable(resp.StatusCode, string(b)) && i+1 < len(candidates) {
				// try next account on same channel
				continue
			}
			res.Classification = "error"
			res.Error = msg
			return res
		}

		var (
			firstTokenAt time.Time
			contentLen   int
			usageOut     int64
			usageReason  int64
		)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				if data == "[DONE]" {
					break
				}
				continue
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) != nil {
				continue
			}
			if u, ok := chunk["usage"].(map[string]any); ok {
				usageOut = anyInt(u["completion_tokens"]) + anyInt(u["output_tokens"])
				usageReason = anyInt(u["reasoning_tokens"])
			}
			choices, _ := chunk["choices"].([]any)
			for _, c := range choices {
				cm, _ := c.(map[string]any)
				delta, _ := cm["delta"].(map[string]any)
				if delta == nil {
					continue
				}
				if t, ok := delta["content"].(string); ok && t != "" {
					if firstTokenAt.IsZero() {
						firstTokenAt = time.Now()
					}
					contentLen += len([]rune(t))
				}
				if t, ok := delta["reasoning_content"].(string); ok && t != "" {
					if firstTokenAt.IsZero() {
						firstTokenAt = time.Now()
					}
					contentLen += len([]rune(t))
				}
			}
		}
		_ = resp.Body.Close()

		duration := time.Since(start)
		res.DurationMs = duration.Milliseconds()
		if !firstTokenAt.IsZero() {
			res.FirstTokenMs = firstTokenAt.Sub(start).Milliseconds()
		}
		outTokens := usageOut + usageReason
		if outTokens <= 0 {
			outTokens = int64(contentLen / 4)
			if outTokens == 0 && contentLen > 0 {
				outTokens = 1
			}
		}
		res.OutputTokens = outTokens
		res.TPS = computeTPS(outTokens, res.DurationMs, res.FirstTokenMs)
		res.Classification = classifyTPS(res.TPS, pol.SoftTPS, pol.HardTPS)
		if res.Classification == "unknown" && outTokens == 0 {
			lastErr = "探测无输出"
			continue
		}
		res.Error = ""
		return res
	}

	res.Classification = "error"
	if lastErr == "" {
		lastErr = "所有候选账号探测失败"
	}
	res.Error = lastErr
	return res
}

func anyInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	default:
		return 0
	}
}

func applyObservation(store *stateStore, nodeID, source string, res qualityResult) {
	pol := store.policy()
	now := float64(time.Now().Unix())
	var (
		doRestore     bool
		doQuarantine  bool
		quarantineWhy string
		nodeCopy      nodeRecord
	)
	updated, err := store.updateNode(nodeID, func(n *nodeRecord) error {
		n.LastClassification = res.Classification
		n.LastOutputTPS = res.TPS
		n.LastFirstTokenMs = res.FirstTokenMs
		n.LastDurationMs = res.DurationMs
		n.LastOutputTokens = res.OutputTokens
		n.LastSource = source
		n.LastObservedAt = now
		if source == "active" {
			n.LastProbeAt = now
		}
		if res.ExitIP != "" {
			n.ExitIP = res.ExitIP
		}
		if res.Error != "" {
			n.LastReason = res.Error
		} else if res.Classification == "healthy" {
			n.LastReason = ""
		}
		switch res.Classification {
		case "healthy":
			n.SoftStrikes = 0
			n.ErrorStrikes = 0
			if n.DisabledByGuard && (source == "active" || pol.Mode == "passive") {
				n.DisabledByGuard = false
				n.QuarantinedUntil = 0
				doRestore = true
			}
		case "soft":
			n.SoftStrikes++
			if n.SoftStrikes >= pol.ConsecutiveSoft && !n.DisabledByGuard {
				doQuarantine = true
				quarantineWhy = fmt.Sprintf("连续软阈值 Token/s=%.1f", res.TPS)
			}
		case "hard":
			if !n.DisabledByGuard {
				doQuarantine = true
				quarantineWhy = fmt.Sprintf("硬阈值 Token/s=%.1f", res.TPS)
			}
		case "error":
			n.ErrorStrikes++
			if n.ErrorStrikes >= pol.ConsecutiveErrors && !n.DisabledByGuard {
				doQuarantine = true
				quarantineWhy = "连续探测错误: " + res.Error
			}
		}
		nodeCopy = *n
		return nil
	})
	if err != nil || updated == nil {
		store.bumpStat(source, res.Classification, res.OutputTokens)
		return
	}
	if doRestore {
		store.bumpAction("restored")
		store.appendEvent(guardEvent{Event: "node_restored", NodeID: nodeCopy.ID, NodeName: nodeCopy.Name, Classification: "healthy", OutputTPS: res.TPS})
		go func(nn nodeRecord) { _ = enableAuthsOnNode(&nn) }(nodeCopy)
	}
	if doQuarantine {
		quarantineNode(store, nodeCopy.ID, quarantineWhy, res.TPS, res.Classification)
	}
	store.bumpStat(source, res.Classification, res.OutputTokens)
}

func quarantineNode(store *stateStore, nodeID, reason string, tps float64, class string) {
	pol := store.policy()
	enabledHealthy := 0
	var target *nodeRecord
	for _, o := range store.listNodes() {
		if o.ID == nodeID {
			target = o
			continue
		}
		if o.Enabled && !o.DisabledByGuard {
			enabledHealthy++
		}
	}
	if target == nil {
		return
	}
	if enabledHealthy < pol.MinHealthyNodes {
		store.bumpAction("suppressed")
		store.appendEvent(guardEvent{Event: "quarantine_suppressed", NodeID: target.ID, NodeName: target.Name, Reason: "低于最低健康节点数", OutputTPS: tps})
		_, _ = store.updateNode(nodeID, func(n *nodeRecord) error {
			n.LastReason = "隔离已抑制: " + reason
			return nil
		})
		return
	}
	updated, err := store.updateNode(nodeID, func(n *nodeRecord) error {
		n.DisabledByGuard = true
		n.QuarantinedUntil = float64(time.Now().Add(time.Duration(pol.QuarantineSec) * time.Second).Unix())
		n.LastReason = reason
		return nil
	})
	if err != nil || updated == nil {
		return
	}
	store.bumpAction("quarantined")
	store.appendEvent(guardEvent{Event: "node_quarantined", NodeID: updated.ID, NodeName: updated.Name, Reason: reason, Classification: class, OutputTPS: tps})
	// Move accounts off the bad channel onto healthy ones (or disable in place).
	go func(nn nodeRecord, why string) {
		if err := migrateAuthsOffNode(store, &nn); err != nil && pol.DisableAuthOnHard {
			_ = disableAuthsOnNode(store, &nn, "egress-guard 降智隔离: "+why)
		}
	}(*updated, reason)
}

func runNodeConnectivity(store *stateStore, id string) (map[string]any, error) {
	n, ok := store.getNode(id)
	if !ok {
		return nil, fmt.Errorf("节点不存在")
	}
	ip, ms, err := probeConnectivity(n.ProxyURL)
	status := "ok"
	if err != nil {
		status = "error"
	}
	_, _ = store.updateNode(id, func(node *nodeRecord) error {
		node.ProbeStatus = status
		node.ProbeLatencyMs = ms
		node.LastProbeAt = float64(time.Now().Unix())
		if ip != "" {
			node.ExitIP = ip
		}
		if err != nil {
			node.LastReason = err.Error()
		}
		return nil
	})
	out := map[string]any{"id": id, "status": status, "exitIp": ip, "latencyMs": ms}
	if err != nil {
		out["error"] = err.Error()
	}
	return out, nil
}

func runNodeQuality(store *stateStore, id string) (map[string]any, error) {
	n, ok := store.getNode(id)
	if !ok {
		return nil, fmt.Errorf("节点不存在")
	}
	if n.DisabledByGuard && n.QuarantinedUntil > float64(time.Now().Unix()) {
		// still allow manual quality test for recovery
	}
	res := probeQuality(store, n)
	applyObservation(store, id, "active", res)
	store.appendEvent(guardEvent{
		Event:          "quality_probe_completed",
		NodeID:         id,
		NodeName:       n.Name,
		Classification: res.Classification,
		OutputTPS:      res.TPS,
		Reason:         res.Error,
	})
	return map[string]any{
		"id":             id,
		"classification": res.Classification,
		"tps":            res.TPS,
		"outputTokens":   res.OutputTokens,
		"durationMs":     res.DurationMs,
		"firstTokenMs":   res.FirstTokenMs,
		"exitIp":         res.ExitIP,
		"error":          res.Error,
		"model":          res.Model,
	}, nil
}

// handlePassiveUsage maps a CPA usage event onto a node by auth proxy_url.
func handlePassiveUsage(store *stateStore, record map[string]any) {
	pol := store.policy()
	authID := firstString(record, "AuthID", "auth_id", "authId", "AuthIndex", "auth_index")
	authIndex := firstString(record, "AuthIndex", "auth_index")
	failed := false
	if v, ok := record["Failed"]; ok {
		failed, _ = v.(bool)
	}
	if v, ok := record["failed"]; ok {
		failed, _ = v.(bool)
	}

	var outTokens, durMs, ttftMs int64
	if detail, ok := record["Detail"].(map[string]any); ok {
		outTokens = anyInt(detail["OutputTokens"]) + anyInt(detail["ReasoningTokens"])
	}
	if detail, ok := record["detail"].(map[string]any); ok {
		if outTokens == 0 {
			outTokens = anyInt(detail["output_tokens"]) + anyInt(detail["reasoning_tokens"]) + anyInt(detail["OutputTokens"])
		}
	}
	if outTokens == 0 {
		outTokens = firstInt(record, "output_tokens", "OutputTokens", "completion_tokens")
	}
	durMs = firstInt(record, "duration_ms", "DurationMs", "latency_ms")
	if durMs == 0 {
		if lat, ok := record["Latency"].(float64); ok {
			// encoding/json decodes time.Duration as nanoseconds
			if lat > 1e6 {
				durMs = int64(lat / 1e6)
			} else {
				durMs = int64(lat)
			}
		}
		if lat, ok := record["latency"].(float64); ok && durMs == 0 {
			if lat > 1e6 {
				durMs = int64(lat / 1e6)
			} else {
				durMs = int64(lat)
			}
		}
	}
	ttftMs = firstInt(record, "first_token_ms", "FirstTokenMs", "ttft_ms")
	if ttftMs == 0 {
		if t, ok := record["TTFT"].(float64); ok {
			if t > 1e6 {
				ttftMs = int64(t / 1e6)
			} else {
				ttftMs = int64(t)
			}
		}
	}

	class := "unknown"
	tps := 0.0
	if failed {
		class = "error"
	} else {
		tps = computeTPS(outTokens, durMs, ttftMs)
		class = classifyTPS(tps, pol.SoftTPS, pol.HardTPS)
		// Very small outputs should not hard-quarantine (loadtest OK replies).
		if class == "hard" && outTokens < 32 {
			class = "healthy"
			tps = 0
		}
	}

	// On anomaly, force auth-proxy cache refresh so we don't miss mappings.
	if class == "hard" || class == "soft" {
		authProxyMu.Lock()
		authProxyAt = time.Time{}
		authProxyMu.Unlock()
	}
	nodeID := resolveNodeIDForAuth(store, authID, authIndex,
		filepath.Base(authID), strings.TrimSuffix(filepath.Base(authID), ".json"))
	res := qualityResult{
		Classification: class,
		TPS:            tps,
		OutputTokens:   outTokens,
		DurationMs:     durMs,
		FirstTokenMs:   ttftMs,
	}
	if nodeID == "" {
		store.bumpStat("passive", class, outTokens)
		if class == "hard" || class == "soft" {
			store.appendEvent(guardEvent{
				Event:          "unmapped_" + class,
				AuthID:         firstNonEmpty(authID, authIndex),
				Classification: class,
				OutputTPS:      tps,
				Reason:         fmt.Sprintf("usage 未映射到出口节点 auth=%s idx=%s tokens=%d dur=%dms ttft=%dms", authID, authIndex, outTokens, durMs, ttftMs),
			})
			// Last resort: attribute hard to the busiest enabled node so we still act.
			if class == "hard" {
				if fallback := busiestEnabledNode(store); fallback != "" {
					store.appendEvent(guardEvent{
						Event:          "hard_fallback_map",
						NodeID:         fallback,
						AuthID:         authID,
						Classification: "hard",
						OutputTPS:      tps,
						Reason:         "未映射账号的硬异常，回退记到负载最高通道并隔离",
					})
					applyObservation(store, fallback, "passive", res)
				}
			}
		}
		return
	}
	// Always apply observation for mapped nodes (quarantine on hard/soft).
	applyObservation(store, nodeID, "passive", res)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func busiestEnabledNode(store *stateStore) string {
	bestID := ""
	bestN := -1
	for _, n := range store.listNodes() {
		if !n.Enabled || n.DisabledByGuard || n.ProxyURL == "" {
			continue
		}
		if n.AssignedAccountCount > bestN {
			bestN = n.AssignedAccountCount
			bestID = n.ID
		}
	}
	return bestID
}

// backgroundWorker periodically probes quarantined / active mode nodes.
func startGuardWorker(ctx context.Context, store *stateStore) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pol := store.policy()
				now := float64(time.Now().Unix())
				for _, n := range store.listNodes() {
					if n.DisabledByGuard && n.QuarantinedUntil > 0 && now >= n.QuarantinedUntil {
						_, _ = runNodeQuality(store, n.ID)
						continue
					}
					if pol.Mode == "active" || pol.Mode == "hybrid" {
						// light active cadence per node via last probe
						if n.Enabled && !n.DisabledByGuard && (n.LastProbeAt == 0 || now-n.LastProbeAt >= float64(pol.ActiveIntervalSec)) {
							// don't stampede — one per tick
							_, _ = runNodeQuality(store, n.ID)
							break
						}
					}
				}
				refreshAssignedCounts(store)
			}
		}
	}()
}
