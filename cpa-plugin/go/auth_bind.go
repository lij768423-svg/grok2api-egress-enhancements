package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

type authFile struct {
	ID       string
	Index    string
	Name     string
	Path     string
	Email    string
	Disabled bool
	ProxyURL string
	Raw      map[string]any
}

func listAuthFiles() ([]authFile, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, mustJSON(map[string]any{}))
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		// some hosts return bare array
		var files []pluginapi.HostAuthFileEntry
		if err2 := json.Unmarshal(raw, &files); err2 != nil {
			return nil, fmt.Errorf("decode auth list: %w", err)
		}
		resp.Files = files
	}
	out := make([]authFile, 0, len(resp.Files))
	for _, f := range resp.Files {
		idx := strings.TrimSpace(f.AuthIndex)
		if idx == "" {
			idx = strings.TrimSpace(f.ID)
		}
		if idx == "" {
			idx = strings.TrimSpace(f.Name)
		}
		if idx == "" {
			continue
		}
		// prefer xai provider/type from list entry
		prov := strings.ToLower(strings.TrimSpace(f.Provider + " " + f.Type + " " + f.Name))
		if prov != "" && !strings.Contains(prov, "xai") {
			continue
		}
		got, err := getAuthFile(idx)
		if err != nil {
			// try by name
			if f.Name != "" {
				got, err = getAuthFile(f.Name)
			}
			if err != nil {
				continue
			}
		}
		if t, _ := got.Raw["type"].(string); strings.ToLower(t) != "xai" && strings.ToLower(t) != "" {
			if !strings.HasPrefix(strings.ToLower(got.Name), "xai-") {
				continue
			}
		}
		got.ID = strings.TrimSpace(f.ID)
		out = append(out, got)
	}
	return out, nil
}

func getAuthFile(authIndex string) (authFile, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthGet, mustJSON(map[string]any{"auth_index": authIndex}))
	if err != nil {
		// try name field
		raw, err = hostCall(pluginabi.MethodHostAuthGet, mustJSON(map[string]any{"name": authIndex}))
		if err != nil {
			return authFile{}, err
		}
	}
	var resp hostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return authFile{}, err
	}
	obj := map[string]any{}
	if len(resp.JSON) > 0 {
		_ = json.Unmarshal(resp.JSON, &obj)
	}
	email, _ := obj["email"].(string)
	proxy, _ := obj["proxy_url"].(string)
	disabled, _ := obj["disabled"].(bool)
	name := resp.Name
	if name == "" {
		name = filepath.Base(resp.Path)
	}
	if name == "" && email != "" {
		name = "xai-" + email + ".json"
	}
	idx := resp.AuthIndex
	if idx == "" {
		idx = authIndex
	}
	return authFile{
		Index:    idx,
		Name:     name,
		Path:     resp.Path,
		Email:    email,
		Disabled: disabled,
		ProxyURL: strings.TrimSpace(proxy),
		Raw:      obj,
	}, nil
}

func saveAuthFile(name string, obj map[string]any) error {
	if name == "" {
		return fmt.Errorf("auth name required")
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = hostCall(pluginabi.MethodHostAuthSave, mustJSON(map[string]any{
		"name": name,
		"json": json.RawMessage(raw),
	}))
	if err == nil {
		invalidateAuthProxyCache()
	}
	return err
}

func setAuthProxyAndFlags(a authFile, proxyURL string, disabled bool, reason string) error {
	if a.Raw == nil {
		a.Raw = map[string]any{}
	}
	if proxyURL == "" {
		delete(a.Raw, "proxy_url")
	} else {
		a.Raw["proxy_url"] = proxyURL
	}
	a.Raw["disabled"] = disabled
	if disabled && reason != "" {
		a.Raw["disabled_reason"] = reason
		a.Raw["disabled_at"] = nowRFC3339()
	} else {
		delete(a.Raw, "disabled_reason")
		delete(a.Raw, "disabled_at")
	}
	// ensure type
	if _, ok := a.Raw["type"]; !ok {
		a.Raw["type"] = "xai"
	}
	return saveAuthFile(a.Name, a.Raw)
}

func isGuardDisabledAuth(a authFile) bool {
	if !a.Disabled {
		return false
	}
	reason, _ := a.Raw["disabled_reason"].(string)
	return strings.Contains(reason, "egress-guard") || strings.Contains(reason, "降智")
}

func verifyAuthBinding(a authFile, expectedProxy string, expectedDisabled bool) error {
	key := firstNonEmpty(a.Index, a.Name)
	if key == "" {
		return fmt.Errorf("账号缺少 auth index")
	}
	got, err := getAuthFile(key)
	if err == nil && got.ProxyURL == expectedProxy && got.Disabled == expectedDisabled {
		return nil
	}
	// A host may regenerate the runtime auth index after host.auth.save. Verify
	// by the stable physical file name before treating the write as failed.
	for _, candidate := range []string{a.Name, filepath.Base(a.Path)} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		for _, listed := range listAuthFilesBestEffort() {
			if listed.Name == candidate && listed.ProxyURL == expectedProxy && listed.Disabled == expectedDisabled {
				return nil
			}
		}
	}
	if err != nil {
		return fmt.Errorf("读取迁移结果失败: %w", err)
	}
	return fmt.Errorf("迁移结果未生效")
}

func listAuthFilesBestEffort() []authFile {
	items, err := listAuthFiles()
	if err != nil {
		return nil
	}
	return items
}

func rebalanceAuthsToNodes(store *stateStore) (map[string]int, error) {
	auths, err := listAuthFiles()
	if err != nil {
		return nil, err
	}
	nodes := store.listNodes()
	// eligible nodes: enabled, not guard-quarantined, has proxy
	eligible := make([]*nodeRecord, 0)
	for _, n := range nodes {
		if n.Enabled && !n.DisabledByGuard && n.ProxyURL != "" {
			eligible = append(eligible, n)
		}
	}
	counts := map[string]int{}
	if len(eligible) == 0 {
		// clear proxies? keep as-is but zero counts
		store.setAssignedCounts(counts)
		return counts, fmt.Errorf("没有可调度出口节点")
	}
	// only rebalance non-disabled auths; disabled stay put
	active := make([]authFile, 0)
	for _, a := range auths {
		if a.Disabled {
			// still count if matches a node proxy
			for _, n := range nodes {
				if a.ProxyURL != "" && a.ProxyURL == n.ProxyURL {
					counts[n.ID]++
				}
			}
			continue
		}
		active = append(active, a)
	}
	// capacity-aware round robin
	cursor := 0
	for _, a := range active {
		// pick next node with capacity room
		var chosen *nodeRecord
		for tried := 0; tried < len(eligible); tried++ {
			n := eligible[cursor%len(eligible)]
			cursor++
			cap := n.AccountCapacity
			if cap > 0 && counts[n.ID] >= cap {
				continue
			}
			chosen = n
			break
		}
		if chosen == nil {
			// all full — pile on last eligible
			chosen = eligible[len(eligible)-1]
		}
		if a.ProxyURL == chosen.ProxyURL && !a.Disabled {
			counts[chosen.ID]++
			continue
		}
		if err := setAuthProxyAndFlags(a, chosen.ProxyURL, false, ""); err != nil {
			return counts, fmt.Errorf("绑定 %s 失败: %w", a.Name, err)
		}
		if err := verifyAuthBinding(a, chosen.ProxyURL, false); err != nil {
			return counts, fmt.Errorf("绑定 %s 校验失败: %w", a.Name, err)
		}
		counts[chosen.ID]++
	}
	store.setAssignedCounts(counts)
	return counts, nil
}

func refreshAssignedCounts(store *stateStore) {
	auths, err := listAuthFiles()
	if err != nil {
		return
	}
	nodes := store.listNodes()
	byProxy := map[string]string{}
	for _, n := range nodes {
		if n.ProxyURL != "" {
			byProxy[n.ProxyURL] = n.ID
		}
	}
	counts := map[string]int{}
	for _, a := range auths {
		if id, ok := byProxy[a.ProxyURL]; ok {
			counts[id]++
		}
	}
	store.setAssignedCounts(counts)
}

func disableAuthsOnNode(store *stateStore, node *nodeRecord, reason string) error {
	if node == nil || node.ProxyURL == "" {
		return nil
	}
	auths, err := listAuthFiles()
	if err != nil {
		return err
	}
	for _, a := range auths {
		if a.ProxyURL == node.ProxyURL && !a.Disabled {
			if err := setAuthProxyAndFlags(a, a.ProxyURL, true, reason); err != nil {
				return fmt.Errorf("停用 %s 失败: %w", a.Name, err)
			}
		}
	}
	return nil
}

func enableAuthsOnNode(node *nodeRecord) error {
	if node == nil || node.ProxyURL == "" {
		return nil
	}
	auths, err := listAuthFiles()
	if err != nil {
		return err
	}
	for _, a := range auths {
		if a.ProxyURL == node.ProxyURL && a.Disabled {
			reason, _ := a.Raw["disabled_reason"].(string)
			if strings.Contains(reason, "egress-guard") || strings.Contains(reason, "降智") {
				_ = setAuthProxyAndFlags(a, a.ProxyURL, false, "")
			}
		}
	}
	return nil
}

func pickAuthForNode(node *nodeRecord) (authFile, error) {
	list, err := listAuthsForNode(node, 1)
	if err != nil {
		return authFile{}, err
	}
	if len(list) == 0 {
		return authFile{}, fmt.Errorf("没有可用的 CPA xAI 账号")
	}
	return list[0], nil
}

// listAuthsForNode returns up to limit enabled xAI auths bound to the node proxy,
// preferring non-expired tokens. Falls back to any enabled xAI auth if none bound.
func listAuthsForNode(node *nodeRecord, limit int) ([]authFile, error) {
	if limit <= 0 {
		limit = 5
	}
	auths, err := listAuthFiles()
	if err != nil {
		return nil, err
	}
	var primary, fallback, expired []authFile
	for _, a := range auths {
		if a.Disabled {
			continue
		}
		tok, _ := a.Raw["access_token"].(string)
		if strings.TrimSpace(tok) == "" {
			continue
		}
		onNode := node != nil && node.ProxyURL != "" && a.ProxyURL == node.ProxyURL
		if isAuthExpired(a) {
			if onNode {
				expired = append(expired, a)
			}
			continue
		}
		if onNode {
			primary = append(primary, a)
		} else {
			fallback = append(fallback, a)
		}
	}
	out := make([]authFile, 0, limit)
	out = append(out, primary...)
	// If node has no fresh bound auth, still try expired bound ones before foreign auths
	// so quality probe still pins to the channel when possible.
	if len(out) == 0 {
		out = append(out, expired...)
	}
	if len(out) < limit {
		out = append(out, fallback...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有可用的 CPA xAI 账号")
	}
	return out, nil
}

// listBoundAuthSummaries returns lightweight account info for a node (no secrets).
func listBoundAuthSummaries(node *nodeRecord) ([]map[string]any, error) {
	auths, err := listAuthFiles()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	if node == nil || node.ProxyURL == "" {
		return out, nil
	}
	for _, a := range auths {
		if a.ProxyURL != node.ProxyURL {
			continue
		}
		out = append(out, map[string]any{
			"name":     a.Name,
			"email":    a.Email,
			"disabled": a.Disabled,
			"expired":  isAuthExpired(a),
		})
	}
	return out, nil
}

func verifiedMigrationTargets(store *stateStore, bad *nodeRecord) []*nodeRecord {
	if store == nil || bad == nil {
		return nil
	}
	pol := store.policy()
	freshness := time.Duration(pol.ActiveIntervalSec*2) * time.Second
	if freshness < time.Hour {
		freshness = time.Hour
	}
	cutoff := float64(time.Now().Add(-freshness).Unix())
	targets := make([]*nodeRecord, 0)
	for _, n := range store.listNodes() {
		if n.ID == bad.ID || !n.Enabled || n.DisabledByGuard || n.ProxyURL == "" {
			continue
		}
		if n.LastClassification != "healthy" || n.LastProbeAt <= cutoff || n.ExitIP == "" {
			continue
		}
		if bad.ExitIP != "" && n.ExitIP == bad.ExitIP {
			continue
		}
		targets = append(targets, n)
	}
	return targets
}

// migrateAuthsOffNode fails closed, then moves only guard-managed accounts to
// recently active-verified nodes with a different observed exit IP.
func migrateAuthsOffNode(store *stateStore, bad *nodeRecord) error {
	if bad == nil || bad.ProxyURL == "" {
		return nil
	}
	auths, err := listAuthFiles()
	if err != nil {
		return err
	}
	affected := make([]authFile, 0)
	for _, a := range auths {
		if a.ProxyURL == bad.ProxyURL && (!a.Disabled || isGuardDisabledAuth(a)) {
			affected = append(affected, a)
		}
	}
	if len(affected) == 0 {
		return nil
	}
	// Remove every affected account from scheduling before changing its proxy.
	for _, a := range affected {
		if a.Disabled {
			continue
		}
		if err := setAuthProxyAndFlags(a, a.ProxyURL, true, "egress-guard 隔离中"); err != nil {
			return fmt.Errorf("隔离账号 %s 失败: %w", a.Name, err)
		}
	}
	healthy := verifiedMigrationTargets(store, bad)
	if len(healthy) == 0 {
		return fmt.Errorf("没有通过主动检测且出口 IP 不同的健康通道")
	}
	cursor := 0
	moved := 0
	failed := 0
	for _, a := range affected {
		dest := healthy[cursor%len(healthy)]
		cursor++
		if err := setAuthProxyAndFlags(a, dest.ProxyURL, false, ""); err != nil || verifyAuthBinding(a, dest.ProxyURL, false) != nil {
			failed++
			continue
		}
		moved++
	}
	refreshAssignedCounts(store)
	if moved > 0 {
		store.appendEvent(guardEvent{
			Event:    "accounts_migrated",
			NodeID:   bad.ID,
			NodeName: bad.Name,
			Reason:   fmt.Sprintf("隔离后迁出 %d 个账号到健康通道，失败 %d 个", moved, failed),
		})
	}
	if failed > 0 {
		return fmt.Errorf("%d 个账号迁移或验证失败", failed)
	}
	return nil
}
