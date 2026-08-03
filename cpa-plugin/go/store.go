package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type policyConfig struct {
	Mode                 string  `json:"mode"`
	ActiveIntervalSec    int     `json:"active_interval_seconds"`
	PassivePollSec       int     `json:"passive_poll_seconds"`
	QuarantineSec        int     `json:"quarantine_seconds"`
	SoftTPS              float64 `json:"soft_tps"`
	HardTPS              float64 `json:"hard_tps"`
	ConsecutiveSoft      int     `json:"consecutive_soft"`
	ConsecutiveErrors    int     `json:"consecutive_errors"`
	MinHealthyNodes      int     `json:"min_healthy_nodes"`
	MinGenerationMs      int64   `json:"min_generation_ms"`
	MinOutputTokens      int64   `json:"min_output_tokens"`
	Model                string  `json:"model"`
	DisableAuthOnHard    bool    `json:"disable_auth_on_hard"`
	MaxOutputTokensProbe int     `json:"max_output_tokens"`
}

type nodeRecord struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	ProxyURL             string    `json:"-"` // never serialize to API clients in clear form via dedicated DTO
	ProxyURLStored       string    `json:"proxy_url"`
	Enabled              bool      `json:"enabled"`
	ProxyPool            bool      `json:"proxy_pool"`
	AccountCapacity      int       `json:"account_capacity"`
	ExitIP               string    `json:"exit_ip,omitempty"`
	ProbeStatus          string    `json:"probe_status,omitempty"`
	ProbeLatencyMs       int64     `json:"probe_latency_ms,omitempty"`
	AssignedAccountCount int       `json:"assigned_account_count"`
	DisabledByGuard      bool      `json:"disabled_by_guard"`
	QuarantinedUntil     float64   `json:"quarantined_until,omitempty"`
	ErrorStrikes         int       `json:"error_strikes"`
	SoftStrikes          int       `json:"soft_strikes"`
	LastClassification   string    `json:"last_classification,omitempty"`
	LastOutputTPS        float64   `json:"last_output_tps,omitempty"`
	LastFirstTokenMs     int64     `json:"last_first_token_ms,omitempty"`
	LastDurationMs       int64     `json:"last_duration_ms,omitempty"`
	LastOutputTokens     int64     `json:"last_output_tokens,omitempty"`
	LastReason           string    `json:"last_reason,omitempty"`
	LastSource           string    `json:"last_source,omitempty"`
	LastObservedAt       float64   `json:"last_observed_at,omitempty"`
	LastProbeAt          float64   `json:"last_probe_at,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type nodeCreateInput struct {
	Name            string
	ProxyURL        string
	Enabled         bool
	ProxyPool       bool
	AccountCapacity int
}

type guardEvent struct {
	TS             float64 `json:"ts"`
	Event          string  `json:"event"`
	NodeID         string  `json:"node_id,omitempty"`
	NodeName       string  `json:"node_name,omitempty"`
	AuthID         string  `json:"auth_id,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	Classification string  `json:"classification,omitempty"`
	OutputTPS      float64 `json:"output_tps,omitempty"`
}

type probeStats struct {
	Total        int64 `json:"total"`
	Healthy      int64 `json:"healthy"`
	Soft         int64 `json:"soft"`
	Hard         int64 `json:"hard"`
	Errors       int64 `json:"errors"`
	Ignored      int64 `json:"ignored"`
	OutputTokens int64 `json:"output_tokens"`
}

type actionStats struct {
	Quarantined int64 `json:"quarantined"`
	Restored    int64 `json:"restored"`
	Suppressed  int64 `json:"suppressed"`
}

type statistics struct {
	StartedAt float64     `json:"started_at"`
	Active    probeStats  `json:"active"`
	Passive   probeStats  `json:"passive"`
	Actions   actionStats `json:"actions"`
}

type guardState struct {
	Version   int                    `json:"version"`
	Policy    policyConfig           `json:"policy"`
	Nodes     map[string]*nodeRecord `json:"nodes"`
	Events    []guardEvent           `json:"events"`
	Stats     statistics             `json:"statistics"`
	NextID    int                    `json:"next_id"`
	UpdatedAt float64                `json:"updated_at"`
}

type stateStore struct {
	mu   sync.Mutex
	path string
	data guardState
}

func defaultPolicy() policyConfig {
	return policyConfig{
		Mode:                 "hybrid",
		ActiveIntervalSec:    1800,
		PassivePollSec:       5,
		QuarantineSec:        120,
		SoftTPS:              500,
		HardTPS:              1000,
		ConsecutiveSoft:      2,
		ConsecutiveErrors:    2,
		MinHealthyNodes:      1,
		MinGenerationMs:      1000,
		MinOutputTokens:      32,
		Model:                "grok-4.5",
		DisableAuthOnHard:    true,
		MaxOutputTokensProbe: 384,
	}
}

func newStateStore(path string) *stateStore {
	s := &stateStore{path: path}
	s.data = guardState{
		Version: 1,
		Policy:  defaultPolicy(),
		Nodes:   map[string]*nodeRecord{},
		Events:  nil,
		Stats:   statistics{StartedAt: float64(time.Now().Unix())},
		NextID:  1,
	}
	_ = s.load()
	return s
}

func (s *stateStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s.persistLocked()
		}
		return err
	}
	var data guardState
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	if data.Nodes == nil {
		data.Nodes = map[string]*nodeRecord{}
	}
	if data.NextID <= 0 {
		data.NextID = 1
	}
	if data.Policy.HardTPS <= 0 {
		data.Policy = defaultPolicy()
	}
	if data.Policy.MinGenerationMs <= 0 {
		data.Policy.MinGenerationMs = 1000
	}
	if data.Policy.MinOutputTokens <= 0 {
		data.Policy.MinOutputTokens = 32
	}
	if data.Policy.MaxOutputTokensProbe <= 0 {
		data.Policy.MaxOutputTokensProbe = 384
	}
	if data.Policy.Mode == "" {
		data.Policy.Mode = "hybrid"
	}
	if data.Policy.ActiveIntervalSec <= 0 {
		data.Policy.ActiveIntervalSec = 1800
	}
	if data.Policy.PassivePollSec <= 0 {
		data.Policy.PassivePollSec = 5
	}
	if data.Policy.QuarantineSec <= 0 {
		data.Policy.QuarantineSec = 120
	}
	// hydrate private proxy field
	for _, n := range data.Nodes {
		n.ProxyURL = n.ProxyURLStored
	}
	s.data = data
	return nil
}

// persistLocked writes state; caller MUST hold s.mu.
func (s *stateStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	s.data.UpdatedAt = float64(time.Now().Unix())
	for _, n := range s.data.Nodes {
		n.ProxyURLStored = n.ProxyURL
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *stateStore) snapshot() guardState {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.data)
	var out guardState
	_ = json.Unmarshal(raw, &out)
	if out.Nodes == nil {
		out.Nodes = map[string]*nodeRecord{}
	}
	for id, n := range s.data.Nodes {
		if out.Nodes[id] != nil {
			out.Nodes[id].ProxyURL = n.ProxyURL
		}
	}
	return out
}

func (s *stateStore) policy() policyConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Policy
}

func (s *stateStore) updatePolicy(p policyConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.SoftTPS <= 0 || p.HardTPS <= 0 || p.SoftTPS >= p.HardTPS {
		return fmt.Errorf("软阈值必须低于硬阈值且都大于 0")
	}
	if p.Mode == "" {
		p.Mode = "hybrid"
	}
	if p.Mode != "active" && p.Mode != "passive" && p.Mode != "hybrid" {
		return fmt.Errorf("模式必须是 active、passive 或 hybrid")
	}
	if p.Model == "" {
		p.Model = "grok-4.5"
	}
	if p.ConsecutiveSoft <= 0 {
		p.ConsecutiveSoft = 2
	}
	if p.ConsecutiveErrors <= 0 {
		p.ConsecutiveErrors = 2
	}
	if p.QuarantineSec <= 0 {
		p.QuarantineSec = 120
	}
	if p.ActiveIntervalSec < 60 || p.ActiveIntervalSec > 86400 {
		return fmt.Errorf("主动检测间隔需在 60 到 86400 秒之间")
	}
	if p.PassivePollSec < 1 || p.PassivePollSec > 3600 {
		return fmt.Errorf("被动审计间隔需在 1 到 3600 秒之间")
	}
	if p.QuarantineSec < 10 || p.QuarantineSec > 86400 {
		return fmt.Errorf("隔离复测间隔需在 10 到 86400 秒之间")
	}
	if p.MinHealthyNodes <= 0 {
		p.MinHealthyNodes = 1
	}
	if p.MinGenerationMs < 200 || p.MinGenerationMs > 10000 {
		return fmt.Errorf("最短生成窗口需在 200 到 10000 毫秒之间")
	}
	if p.MinOutputTokens < 1 || p.MinOutputTokens > 10000 {
		return fmt.Errorf("最小判定 Token 数需在 1 到 10000 之间")
	}
	if p.MaxOutputTokensProbe < 16 || p.MaxOutputTokensProbe > 4096 {
		return fmt.Errorf("主动探测最大输出需在 16 到 4096 Token 之间")
	}
	s.data.Policy = p
	return s.persistLocked()
}

func (s *stateStore) listNodes() []*nodeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*nodeRecord, 0, len(s.data.Nodes))
	for _, n := range s.data.Nodes {
		cp := *n
		cp.ProxyURL = n.ProxyURL
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *stateStore) getNode(id string) (*nodeRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.data.Nodes[id]
	if !ok {
		return nil, false
	}
	cp := *n
	cp.ProxyURL = n.ProxyURL
	return &cp, true
}

func (s *stateStore) createNode(name, proxyURL string, enabled, pool bool, capacity int) (*nodeRecord, error) {
	created, err := s.createNodes([]nodeCreateInput{{
		Name:            name,
		ProxyURL:        proxyURL,
		Enabled:         enabled,
		ProxyPool:       pool,
		AccountCapacity: capacity,
	}})
	if err != nil {
		return nil, err
	}
	return created[0], nil
}

func (s *stateStore) createNodes(inputs []nodeCreateInput) ([]*nodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(inputs) == 0 {
		return nil, fmt.Errorf("至少提供一个节点")
	}
	if len(inputs) > 500 {
		return nil, fmt.Errorf("单次最多导入 500 个节点")
	}
	for index := range inputs {
		inputs[index].Name = strings.TrimSpace(inputs[index].Name)
		inputs[index].ProxyURL = strings.TrimSpace(inputs[index].ProxyURL)
		if inputs[index].Name == "" || inputs[index].ProxyURL == "" {
			return nil, fmt.Errorf("第 %d 个节点缺少名称或代理 URL", index+1)
		}
		if err := validateProxyURL(inputs[index].ProxyURL); err != nil {
			return nil, fmt.Errorf("第 %d 个节点代理 URL 无效: %w", index+1, err)
		}
		if inputs[index].AccountCapacity < 0 || inputs[index].AccountCapacity > 100000 {
			return nil, fmt.Errorf("第 %d 个节点容量需在 0 到 100000 之间", index+1)
		}
	}
	previousNextID := s.data.NextID
	now := time.Now().UTC()
	created := make([]*nodeRecord, 0, len(inputs))
	createdIDs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		id := fmt.Sprintf("%d", s.data.NextID)
		s.data.NextID++
		n := &nodeRecord{
			ID:              id,
			Name:            input.Name,
			ProxyURL:        input.ProxyURL,
			ProxyURLStored:  input.ProxyURL,
			Enabled:         input.Enabled,
			ProxyPool:       input.ProxyPool,
			AccountCapacity: input.AccountCapacity,
			ProbeStatus:     "unknown",
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		s.data.Nodes[id] = n
		createdIDs = append(createdIDs, id)
		cp := *n
		created = append(created, &cp)
	}
	if err := s.persistLocked(); err != nil {
		for _, id := range createdIDs {
			delete(s.data.Nodes, id)
		}
		s.data.NextID = previousNextID
		return nil, err
	}
	return created, nil
}

func (s *stateStore) updateNode(id string, mut func(*nodeRecord) error) (*nodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.data.Nodes[id]
	if !ok {
		return nil, fmt.Errorf("节点不存在")
	}
	if err := mut(n); err != nil {
		return nil, err
	}
	if n.ProxyURL != "" {
		if err := validateProxyURL(n.ProxyURL); err != nil {
			return nil, err
		}
	}
	n.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	cp := *n
	cp.ProxyURL = n.ProxyURL
	return &cp, nil
}

func validateProxyURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("代理 URL 必须包含主机和端口")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return fmt.Errorf("代理协议仅支持 http、https、socks5 或 socks5h")
	}
}

func (s *stateStore) deleteNodes(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.data.Nodes, id)
	}
	return s.persistLocked()
}

func (s *stateStore) setBatchEnabled(ids []string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if n, ok := s.data.Nodes[id]; ok {
			n.Enabled = enabled
			n.UpdatedAt = time.Now().UTC()
		}
	}
	return s.persistLocked()
}

func (s *stateStore) appendEvent(ev guardEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.TS == 0 {
		ev.TS = float64(time.Now().Unix())
	}
	s.data.Events = append(s.data.Events, ev)
	if len(s.data.Events) > 100 {
		s.data.Events = s.data.Events[len(s.data.Events)-100:]
	}
	_ = s.persistLocked()
}

func (s *stateStore) events() []guardEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]guardEvent, len(s.data.Events))
	copy(out, s.data.Events)
	return out
}

func (s *stateStore) stats() statistics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Stats
}

func (s *stateStore) bumpStat(source, class string, tokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ps *probeStats
	if source == "active" {
		ps = &s.data.Stats.Active
	} else {
		ps = &s.data.Stats.Passive
	}
	ps.Total++
	ps.OutputTokens += tokens
	switch class {
	case "healthy":
		ps.Healthy++
	case "soft":
		ps.Soft++
	case "hard":
		ps.Hard++
	case "error":
		ps.Errors++
	case "ignored", "account_error", "upstream_error", "no_account":
		ps.Ignored++
	}
	_ = s.persistLocked()
}

func (s *stateStore) bumpAction(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "quarantined":
		s.data.Stats.Actions.Quarantined++
	case "restored":
		s.data.Stats.Actions.Restored++
	case "suppressed":
		s.data.Stats.Actions.Suppressed++
	}
	_ = s.persistLocked()
}

func (s *stateStore) setAssignedCounts(counts map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, n := range s.data.Nodes {
		n.AssignedAccountCount = counts[id]
	}
	_ = s.persistLocked()
}

func publicNode(n *nodeRecord) map[string]any {
	if n == nil {
		return nil
	}
	return map[string]any{
		"id":                   n.ID,
		"name":                 n.Name,
		"enabled":              n.Enabled,
		"proxyPool":            n.ProxyPool,
		"accountCapacity":      n.AccountCapacity,
		"exitIp":               n.ExitIP,
		"probeStatus":          n.ProbeStatus,
		"probeLatencyMs":       n.ProbeLatencyMs,
		"assignedAccountCount": n.AssignedAccountCount,
		"disabled_by_guard":    n.DisabledByGuard,
		"quarantined_until":    n.QuarantinedUntil,
		"error_strikes":        n.ErrorStrikes,
		"soft_strikes":         n.SoftStrikes,
		"last_classification":  n.LastClassification,
		"last_output_tps":      n.LastOutputTPS,
		"last_first_token_ms":  n.LastFirstTokenMs,
		"last_duration_ms":     n.LastDurationMs,
		"last_output_tokens":   n.LastOutputTokens,
		"last_reason":          n.LastReason,
		"last_source":          n.LastSource,
		"last_observed_at":     n.LastObservedAt,
		"last_probe_at":        n.LastProbeAt,
		"hasProxy":             n.ProxyURL != "",
		"createdAt":            n.CreatedAt,
		"updatedAt":            n.UpdatedAt,
	}
}
