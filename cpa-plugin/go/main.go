package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName          = "grok2api-egress"
	pluginVersion       = "1.0.5"
	resourcePath        = "/status"
	managementAPIPath   = "/v0/management/grok2api-egress/api"
	resourceContentType = "text/html; charset=utf-8"
	defaultStateFile    = "/CLIProxyAPI/plugin-data/egress-guard/state.json"
)

//go:embed page.html
var pageTemplate string

//go:embed tokens.css
var tokenCSS string

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	StateFile          string   `yaml:"state_file" json:"state_file"`
	RotationURL        string   `yaml:"rotation_url" json:"rotation_url"`
	RotationTokenEnv   string   `yaml:"rotation_token_env" json:"rotation_token_env"`
	RotationTimeoutSec int      `yaml:"rotation_timeout_seconds" json:"rotation_timeout_seconds"`
	RotatableNodeIDs   []string `yaml:"rotatable_node_ids" json:"rotatable_node_ids"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ManagementAPI      bool `json:"management_api"`
	UsagePlugin        bool `json:"usage_plugin"`
	Scheduler          bool `json:"scheduler"`
	RequestInterceptor bool `json:"request_interceptor"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

type uiProxyRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

var (
	store         *stateStore
	workerCancel  context.CancelFunc
	currentConfig atomic.Value // pluginConfig
	startedAt     = time.Now().UTC()
	// hostCall is replaceable in unit tests. Production always uses the C ABI
	// callback implemented by callHost below.
	hostCall = callHost
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	currentConfig.Store(pluginConfig{StateFile: defaultStateFile})
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	if workerCancel != nil {
		workerCancel()
	}
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Routes:    []managementRoute{{Method: http.MethodPost, Path: "/grok2api-egress/api", Description: "CPA 出口守护 UI API"}},
			Resources: []managementResource{{Path: resourcePath, Menu: "出口守护", Description: "纯 CPA 出口节点 · 降智隔离 · 质量检测（不依赖 Grok2API）"}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodRequestInterceptBefore:
		return handleRequestIntercept(request, false)
	case pluginabi.MethodRequestInterceptAfter:
		return handleRequestIntercept(request, true)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := pluginConfig{StateFile: defaultStateFile}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.StateFile) == "" {
		cfg.StateFile = defaultStateFile
	}
	if cfg.RotationTimeoutSec <= 0 {
		cfg.RotationTimeoutSec = 45
	}
	currentConfig.Store(cfg)
	store = newStateStore(cfg.StateFile)
	if workerCancel != nil {
		workerCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	workerCancel = cancel
	startGuardWorker(ctx, store)
	refreshAssignedCounts(store)
	return nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "lij768423-svg",
			GitHubRepository: "https://github.com/lij768423-svg/grok2api-egress-enhancements",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "state_file", Type: pluginapi.ConfigFieldTypeString, Description: "出口守护状态文件路径（节点/策略/事件）"},
				{Name: "rotation_url", Type: pluginapi.ConfigFieldTypeString, Description: "可选、受信任的内部换 IP Webhook；仅对 rotatable_node_ids 生效"},
				{Name: "rotation_token_env", Type: pluginapi.ConfigFieldTypeString, Description: "从 CPA 进程环境变量读取 Webhook Bearer Token，避免写入配置"},
				{Name: "rotation_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "换 IP Webhook 超时（秒）"},
				{Name: "rotatable_node_ids", Type: pluginapi.ConfigFieldTypeArray, Description: "允许自动换 IP 的节点 ID；留空时禁止自动换 IP"},
			},
		},
		Capabilities: registrationCapabilities{ManagementAPI: true, UsagePlugin: true, Scheduler: true, RequestInterceptor: true},
	}
}

func handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = resourcePath
	}
	base := "/v0/resource/plugins/" + pluginName
	switch {
	case path == resourcePath, path == "/", path == base, path == base+"/", path == base+resourcePath, strings.HasSuffix(path, "/status"):
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"content-type": []string{resourceContentType}},
			Body:       []byte(strings.Replace(pageTemplate, "/*__HALLMARK_TOKENS__*/", tokenCSS, 1)),
		})
	case path == managementAPIPath:
		return handleUIProxy(req)
	default:
		return okEnvelope(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"content-type": []string{"text/plain; charset=utf-8"}},
			Body:       []byte("not found"),
		})
	}
}

func handleUIProxy(req managementRequest) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodPost) || req.Headers.Get("X-Grok2API-Egress-UI") != "1" {
		return managementJSON(http.StatusForbidden, map[string]any{"error": map[string]string{"code": "forbidden", "message": "forbidden"}})
	}
	var input uiProxyRequest
	if len(req.Body) == 0 || json.Unmarshal(req.Body, &input) != nil {
		return managementJSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalidRequest", "message": "invalid request"}})
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(input.Path))
	if err != nil || parsed.IsAbs() || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/") {
		return managementJSON(http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalidPath", "message": "invalid path"}})
	}
	method := strings.ToUpper(strings.TrimSpace(input.Method))
	if method == "" {
		method = http.MethodGet
	}
	return dispatchAPI(method, parsed.Path, parsed.Query(), input.Body)
}

func dispatchAPI(method, path string, query url.Values, body json.RawMessage) ([]byte, error) {
	ensureStore()
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")

	switch {
	case path == "/status" || path == "/quality-guard":
		if method != http.MethodGet {
			return managementJSON(http.StatusMethodNotAllowed, errMsg("methodNotAllowed", "method not allowed"))
		}
		return managementJSON(http.StatusOK, buildStatus())

	case path == "/policy" || path == "/quality-guard/config":
		if method == http.MethodGet {
			return managementJSON(http.StatusOK, map[string]any{"data": store.policy(), "config": store.policy()})
		}
		if method == http.MethodPut || method == http.MethodPost {
			var p policyConfig
			// accept both snake and camel
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("invalidBody", "invalid body"))
			}
			p = store.policy()
			if v, ok := raw["mode"].(string); ok {
				p.Mode = v
			}
			p.ActiveIntervalSec = intPick(raw, p.ActiveIntervalSec, "active_interval_seconds", "activeIntervalSeconds")
			p.PassivePollSec = intPick(raw, p.PassivePollSec, "passive_poll_seconds", "passivePollSeconds")
			p.QuarantineSec = intPick(raw, p.QuarantineSec, "quarantine_seconds", "quarantineSeconds")
			p.SoftTPS = floatPick(raw, p.SoftTPS, "soft_tps", "softTPS")
			p.HardTPS = floatPick(raw, p.HardTPS, "hard_tps", "hardTPS")
			p.ConsecutiveSoft = intPick(raw, p.ConsecutiveSoft, "consecutive_soft", "consecutiveSoft")
			p.ConsecutiveErrors = intPick(raw, p.ConsecutiveErrors, "consecutive_errors", "consecutiveErrors")
			p.MinHealthyNodes = intPick(raw, p.MinHealthyNodes, "min_healthy_nodes", "minHealthyNodes")
			p.MinGenerationMs = int64(intPick(raw, int(p.MinGenerationMs), "min_generation_ms", "minGenerationMs"))
			p.MinOutputTokens = int64(intPick(raw, int(p.MinOutputTokens), "min_output_tokens", "minOutputTokens"))
			p.MaxOutputTokensProbe = intPick(raw, p.MaxOutputTokensProbe, "max_output_tokens", "maxOutputTokens")
			if v, ok := raw["model"].(string); ok && v != "" {
				p.Model = v
			}
			if v, ok := raw["disable_auth_on_hard"].(bool); ok {
				p.DisableAuthOnHard = v
			}
			if err := store.updatePolicy(p); err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("invalidPolicy", err.Error()))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": store.policy(), "ok": true})
		}

	case path == "/nodes":
		if method == http.MethodGet {
			refreshAssignedCounts(store)
			items := store.listNodes()
			out := make([]map[string]any, 0, len(items))
			for _, n := range items {
				out = append(out, publicNode(n))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": map[string]any{"items": out, "total": len(out)}, "items": out, "total": len(out)})
		}
		if method == http.MethodPost {
			var raw map[string]any
			_ = json.Unmarshal(body, &raw)
			name, _ := raw["name"].(string)
			proxy, _ := raw["proxyURL"].(string)
			if proxy == "" {
				proxy, _ = raw["proxy_url"].(string)
			}
			enabled := true
			if v, ok := raw["enabled"].(bool); ok {
				enabled = v
			}
			pool, _ := raw["proxyPool"].(bool)
			if !pool {
				pool, _ = raw["proxy_pool"].(bool)
			}
			cap := intPick(raw, 0, "accountCapacity", "account_capacity")
			n, err := store.createNode(strings.TrimSpace(name), strings.TrimSpace(proxy), enabled, pool, cap)
			if err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("createFailed", err.Error()))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": publicNode(n)})
		}
		if method == http.MethodDelete {
			var raw map[string]any
			_ = json.Unmarshal(body, &raw)
			ids := stringIDs(raw["ids"])
			// unbind auths on those nodes first
			for _, id := range ids {
				if n, ok := store.getNode(id); ok {
					auths, _ := listAuthFiles()
					for _, a := range auths {
						if a.ProxyURL == n.ProxyURL {
							_ = setAuthProxyAndFlags(a, "", a.Disabled, "")
						}
					}
				}
			}
			_ = store.deleteNodes(ids)
			return managementJSON(http.StatusOK, map[string]any{"ok": true, "deleted": len(ids)})
		}

	case path == "/nodes/batch":
		if method == http.MethodPatch || method == http.MethodPost {
			var raw map[string]any
			_ = json.Unmarshal(body, &raw)
			ids := stringIDs(raw["ids"])
			if v, ok := raw["enabled"].(bool); ok {
				_ = store.setBatchEnabled(ids, v)
			}
			return managementJSON(http.StatusOK, map[string]any{"ok": true})
		}

	case path == "/nodes/import":
		if method == http.MethodPost {
			var raw struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(body, &raw); err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("invalidBody", "批量节点数据无效"))
			}
			if len(raw.Items) == 0 || len(raw.Items) > 500 {
				return managementJSON(http.StatusBadRequest, errMsg("invalidBody", "单次需导入 1 到 500 个节点"))
			}
			inputs := make([]nodeCreateInput, 0, len(raw.Items))
			for index, item := range raw.Items {
				name, _ := item["name"].(string)
				proxy, _ := item["proxyURL"].(string)
				if proxy == "" {
					proxy, _ = item["proxy_url"].(string)
				}
				if strings.TrimSpace(name) == "" {
					name = fmt.Sprintf("Node %03d", index+1)
				}
				enabled := true
				if value, ok := item["enabled"].(bool); ok {
					enabled = value
				}
				pool, _ := item["proxyPool"].(bool)
				if !pool {
					pool, _ = item["proxy_pool"].(bool)
				}
				inputs = append(inputs, nodeCreateInput{
					Name:            name,
					ProxyURL:        proxy,
					Enabled:         enabled,
					ProxyPool:       pool,
					AccountCapacity: intPick(item, 0, "accountCapacity", "account_capacity"),
				})
			}
			created, err := store.createNodes(inputs)
			if err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("importFailed", err.Error()))
			}
			items := make([]map[string]any, 0, len(created))
			for _, node := range created {
				items = append(items, publicNode(node))
			}
			return managementJSON(http.StatusOK, map[string]any{
				"ok":      true,
				"data":    map[string]any{"items": items, "created": len(items)},
				"items":   items,
				"created": len(items),
			})
		}

	case path == "/nodes/test":
		if method == http.MethodPost {
			var raw map[string]any
			_ = json.Unmarshal(body, &raw)
			ids := stringIDs(raw["ids"])
			results := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				r, err := runNodeConnectivity(store, id)
				if err != nil {
					results = append(results, map[string]any{"id": id, "error": err.Error()})
				} else {
					results = append(results, r)
				}
			}
			return managementJSON(http.StatusOK, map[string]any{"data": results, "results": results})
		}

	case path == "/nodes/rebalance" || path == "/rebalance":
		if method == http.MethodPost {
			counts, err := rebalanceAuthsToNodes(store)
			if err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("rebalanceFailed", err.Error()))
			}
			return managementJSON(http.StatusOK, map[string]any{"ok": true, "counts": counts})
		}

	case len(parts) == 2 && parts[0] == "nodes" && safeID(parts[1]):
		id := parts[1]
		if method == http.MethodGet {
			n, ok := store.getNode(id)
			if !ok {
				return managementJSON(http.StatusNotFound, errMsg("notFound", "not found"))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": publicNode(n)})
		}
		if method == http.MethodPut || method == http.MethodPatch {
			var raw map[string]any
			_ = json.Unmarshal(body, &raw)
			n, err := store.updateNode(id, func(node *nodeRecord) error {
				if v, ok := raw["name"].(string); ok && strings.TrimSpace(v) != "" {
					node.Name = strings.TrimSpace(v)
				}
				if v, ok := raw["enabled"].(bool); ok {
					node.Enabled = v
				}
				if v, ok := raw["proxyPool"].(bool); ok {
					node.ProxyPool = v
				}
				if v, ok := raw["proxy_pool"].(bool); ok {
					node.ProxyPool = v
				}
				if _, ok := raw["accountCapacity"]; ok {
					node.AccountCapacity = intPick(raw, node.AccountCapacity, "accountCapacity", "account_capacity")
				}
				proxy, _ := raw["proxyURL"].(string)
				if proxy == "" {
					proxy, _ = raw["proxy_url"].(string)
				}
				if strings.TrimSpace(proxy) != "" {
					node.ProxyURL = strings.TrimSpace(proxy)
				}
				return nil
			})
			if err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("updateFailed", err.Error()))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": publicNode(n)})
		}
		if method == http.MethodDelete {
			if n, ok := store.getNode(id); ok {
				auths, _ := listAuthFiles()
				for _, a := range auths {
					if a.ProxyURL == n.ProxyURL {
						_ = setAuthProxyAndFlags(a, "", a.Disabled, "")
					}
				}
			}
			_ = store.deleteNodes([]string{id})
			return managementJSON(http.StatusOK, map[string]any{"ok": true})
		}

	case len(parts) == 3 && parts[0] == "nodes" && safeID(parts[1]) && parts[2] == "test":
		if method == http.MethodPost {
			r, err := runNodeConnectivity(store, parts[1])
			if err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("testFailed", err.Error()))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": r})
		}

	case len(parts) == 3 && parts[0] == "nodes" && safeID(parts[1]) && parts[2] == "accounts":
		if method == http.MethodGet {
			n, ok := store.getNode(parts[1])
			if !ok {
				return managementJSON(http.StatusNotFound, errMsg("notFound", "not found"))
			}
			items, err := listBoundAuthSummaries(n)
			if err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("listFailed", err.Error()))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": map[string]any{"items": items, "total": len(items)}, "items": items, "total": len(items)})
		}

	case len(parts) == 3 && parts[0] == "nodes" && safeID(parts[1]) && (parts[2] == "quality-test" || parts[2] == "quality"):
		if method == http.MethodPost {
			r, err := runNodeQuality(store, parts[1])
			if err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("qualityFailed", err.Error()))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": r})
		}
	case len(parts) == 4 && parts[0] == "quality-guard" && parts[1] == "nodes" && safeID(parts[2]) && parts[3] == "test":
		if method == http.MethodPost {
			r, err := runNodeQuality(store, parts[2])
			if err != nil {
				return managementJSON(http.StatusBadRequest, errMsg("qualityFailed", err.Error()))
			}
			return managementJSON(http.StatusOK, map[string]any{"data": r})
		}
	}

	return managementJSON(http.StatusNotFound, errMsg("notFound", "not found"))
}

func buildStatus() map[string]any {
	ensureStore()
	refreshAssignedCounts(store)
	nodes := store.listNodes()
	nodeMap := map[string]any{}
	for _, n := range nodes {
		nodeMap[n.ID] = map[string]any{
			"disabled_by_guard":   n.DisabledByGuard,
			"quarantined_until":   n.QuarantinedUntil,
			"error_strikes":       n.ErrorStrikes,
			"soft_strikes":        n.SoftStrikes,
			"last_classification": n.LastClassification,
			"last_output_tps":     n.LastOutputTPS,
			"last_first_token_ms": n.LastFirstTokenMs,
			"last_duration_ms":    n.LastDurationMs,
			"last_output_tokens":  n.LastOutputTokens,
			"last_reason":         n.LastReason,
			"last_source":         n.LastSource,
			"last_observed_at":    n.LastObservedAt,
			"last_probe_at":       n.LastProbeAt,
		}
	}
	pol := store.policy()
	st := store.stats()
	return map[string]any{
		"available":    true,
		"updatedAt":    store.snapshot().UpdatedAt,
		"config":       pol,
		"editable":     true,
		"nodes":        nodeMap,
		"statistics":   st,
		"recentEvents": store.events(),
		"plugin":       pluginName,
		"version":      pluginVersion,
		"started_at":   startedAt.Format(time.RFC3339),
		"engine":       "cpa-native",
		"hint":         "纯 CPA 出口守护：节点代理写在账号 proxy_url，被动 Token/s 审计 + 主动质量探测，不依赖 Grok2API。",
	}
}

func handleUsage(request []byte) ([]byte, error) {
	ensureStore()
	var payload map[string]any
	if len(request) > 0 {
		_ = json.Unmarshal(request, &payload)
	}
	// Also accept nested record
	if rec, ok := payload["record"].(map[string]any); ok {
		payload = rec
	}
	handlePassiveUsage(store, payload)
	return okEnvelope(map[string]any{"recorded": true})
}

func ensureStore() {
	if store == nil {
		cfg := pluginConfig{StateFile: defaultStateFile}
		if v := currentConfig.Load(); v != nil {
			if c, ok := v.(pluginConfig); ok {
				cfg = c
			}
		}
		store = newStateStore(cfg.StateFile)
	}
}

func managementJSON(status int, v any) ([]byte, error) {
	body, _ := json.Marshal(v)
	// UI expects payload.data — also top-level
	return okEnvelope(managementResponse{
		StatusCode: status,
		Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	})
}

func errMsg(code, message string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": message}}
}

func safeID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func stringIDs(v any) []string {
	out := []string{}
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			out = append(out, fmt.Sprint(x))
		}
	case []string:
		out = append(out, t...)
	}
	return out
}

func intPick(raw map[string]any, def int, keys ...string) int {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			return int(anyInt(v))
		}
	}
	return def
}

func floatPick(raw map[string]any, def float64, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			switch t := v.(type) {
			case float64:
				return t
			case int:
				return float64(t)
			case json.Number:
				f, _ := t.Float64()
				return f
			}
		}
	}
	return def
}

func firstString(payload map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := payload[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	// nested
	for _, wrap := range []string{"usage", "meta", "request", "data"} {
		if m, ok := payload[wrap].(map[string]any); ok {
			if s := firstString(m, keys...); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstInt(payload map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := payload[k]; ok {
			if n := anyInt(v); n != 0 {
				return n
			}
		}
	}
	return 0
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func callHost(method string, payload []byte) (json.RawMessage, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var reqPtr *C.uint8_t
	if len(payload) > 0 {
		reqPtr = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(reqPtr))
	}
	code := C.call_host_api(cMethod, reqPtr, C.size_t(len(payload)), &response)
	if code != 0 {
		return nil, fmt.Errorf("host callback %s code=%d", method, int(code))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, fmt.Errorf("host callback %s empty", method)
	}
	raw := C.GoBytes(response.ptr, C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw, nil
	}
	if !env.OK {
		msg := "host error"
		if env.Error != nil {
			msg = env.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil {
		return
	}
	if len(raw) == 0 {
		response.ptr = nil
		response.len = 0
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

// silence unused html import used by tests/templates indirectly
var _ = html.EscapeString
