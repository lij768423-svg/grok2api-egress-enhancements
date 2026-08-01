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
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName           = "grok2api-egress"
	pluginVersion        = "0.4.0"
	resourcePath         = "/status"
	managementAPIPath    = "/v0/management/grok2api-egress/api"
	resourceContentType  = "text/html; charset=utf-8"
	defaultGrok2APIBase  = "http://127.0.0.1:8181"
	defaultHardTPS       = 1000.0
	defaultSoftTPS       = 500.0
	maxRecentEvents      = 40
	maxProxyRequestBody  = 2 << 20
	maxProxyResponseBody = 8 << 20
)

//go:embed page.html
var pageTemplate string

//go:embed tokens.css
var tokenCSS string

//go:embed app.css
var appCSS string

//go:embed app.js
var appJS string

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
	Grok2APIBaseURL string  `yaml:"grok2api_base_url"`
	HardTPS         float64 `yaml:"hard_tps"`
	SoftTPS         float64 `yaml:"soft_tps"`
	DisableOnHard   bool    `yaml:"disable_on_hard"`
	FetchTimeoutSec int     `yaml:"fetch_timeout_sec"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ManagementAPI bool `json:"management_api"`
	UsagePlugin   bool `json:"usage_plugin"`
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

type usageEvent struct {
	At           time.Time `json:"at"`
	AuthID       string    `json:"auth_id,omitempty"`
	Model        string    `json:"model,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	OutputTokens int64     `json:"output_tokens"`
	DurationMs   int64     `json:"duration_ms"`
	FirstTokenMs int64     `json:"first_token_ms"`
	TPS          float64   `json:"tps"`
	Class        string    `json:"class"`
	Error        string    `json:"error,omitempty"`
}

type authStats struct {
	AuthID      string    `json:"auth_id"`
	Count       int64     `json:"count"`
	HardHits    int64     `json:"hard_hits"`
	SoftHits    int64     `json:"soft_hits"`
	ErrorHits   int64     `json:"error_hits"`
	LastTPS     float64   `json:"last_tps"`
	LastClass   string    `json:"last_class"`
	LastSeen    time.Time `json:"last_seen"`
	TotalOutTok int64     `json:"total_output_tokens"`
	TotalDurMs  int64     `json:"total_duration_ms"`
}

var (
	currentConfig atomic.Value // pluginConfig
	statsMu       sync.Mutex
	recentEvents  []usageEvent
	byAuth        = map[string]*authStats{}
	totalHandled  int64
	startedAt     = time.Now().UTC()
	grokAdmin     adminSession
)

type adminSession struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	currentConfig.Store(defaultConfig())
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
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Routes:    []managementRoute{{Method: http.MethodPost, Path: "/grok2api-egress/api", Description: "Authenticated Grok2API Egress UI API bridge"}},
			Resources: []managementResource{{Path: resourcePath, Menu: "Grok2API Egress", Description: "出口质量守护 + 固定代理恢复（CPA 插件视图）"}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Grok2APIBaseURL: defaultGrok2APIBase,
		HardTPS:         defaultHardTPS,
		SoftTPS:         defaultSoftTPS,
		DisableOnHard:   false,
		FetchTimeoutSec: 4,
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if errYAML := yaml.Unmarshal(req.ConfigYAML, &cfg); errYAML != nil {
			return errYAML
		}
	}
	if strings.TrimSpace(cfg.Grok2APIBaseURL) == "" {
		cfg.Grok2APIBaseURL = defaultGrok2APIBase
	}
	if cfg.HardTPS <= 0 {
		cfg.HardTPS = defaultHardTPS
	}
	if cfg.SoftTPS <= 0 {
		cfg.SoftTPS = defaultSoftTPS
	}
	if cfg.FetchTimeoutSec <= 0 {
		cfg.FetchTimeoutSec = 4
	}
	cfg.Grok2APIBaseURL = strings.TrimRight(strings.TrimSpace(cfg.Grok2APIBaseURL), "/")
	currentConfig.Store(cfg)
	grokAdmin.reset()
	return nil
}

func loadConfig() pluginConfig {
	if v := currentConfig.Load(); v != nil {
		if cfg, ok := v.(pluginConfig); ok {
			return cfg
		}
	}
	return defaultConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "lij768423-svg",
			GitHubRepository: "https://github.com/lij768423-svg/grok2api-egress-enhancements",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "grok2api_base_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "grok2api base URL used by the status page (default http://127.0.0.1:8181).",
				},
				{
					Name:        "hard_tps",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Description: "Passive hard TPS threshold (outputTokens / (durationMs - firstTokenMs) * 1000).",
				},
				{
					Name:        "soft_tps",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Description: "Passive soft TPS threshold for consecutive soft strikes.",
				},
				{
					Name:        "disable_on_hard",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "When true, hard TPS hits mark auth disabled via host.auth.save if possible.",
				},
				{
					Name:        "fetch_timeout_sec",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Description: "HTTP timeout seconds when probing grok2api from the status page.",
				},
			},
		},
		Capabilities: registrationCapabilities{
			ManagementAPI: true,
			UsagePlugin:   true,
		},
	}
}

func handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = resourcePath
	}
	// CPA passes the full browser path: /v0/resource/plugins/<id>/...
	base := "/v0/resource/plugins/" + pluginName
	switch {
	case path == resourcePath, path == "/", path == base, path == base+"/", path == base+resourcePath, strings.HasSuffix(path, "/status"):
		body := renderStatusPage()
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"content-type": []string{resourceContentType},
			},
			Body: body,
		})
	case path == "/api/snapshot", path == base+"/api/snapshot", strings.HasSuffix(path, "/api/snapshot"):
		snap := buildSnapshot()
		raw, errMarshal := json.MarshalIndent(snap, "", "  ")
		if errMarshal != nil {
			return nil, errMarshal
		}
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"content-type": []string{"application/json; charset=utf-8"},
			},
			Body: raw,
		})
	case path == managementAPIPath:
		return handleUIProxy(req, base)
	case isGrokProxyRequest(path, base):
		return proxyGrokManagement(req, path, base)
	default:
		return okEnvelope(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers: http.Header{
				"content-type": []string{"text/plain; charset=utf-8"},
			},
			Body: []byte("not found"),
		})
	}
}

func handleUIProxy(req managementRequest, base string) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodPost) || req.Headers.Get("X-Grok2API-Egress-UI") != "1" {
		return managementJSONResponse(http.StatusForbidden, []byte(`{"error":{"code":"forbidden","message":"forbidden"}}`))
	}
	var input uiProxyRequest
	if len(req.Body) == 0 || json.Unmarshal(req.Body, &input) != nil {
		return managementJSONResponse(http.StatusBadRequest, []byte(`{"error":{"code":"invalidRequest","message":"invalid request"}}`))
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(input.Path))
	if err != nil || parsed.IsAbs() || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/") {
		return managementJSONResponse(http.StatusBadRequest, []byte(`{"error":{"code":"invalidPath","message":"invalid path"}}`))
	}
	proxyRequest := managementRequest{
		Method: strings.ToUpper(strings.TrimSpace(input.Method)),
		Path:   base + "/api/grok2api" + parsed.Path,
		Query:  parsed.Query(),
		Body:   append([]byte(nil), input.Body...),
	}
	return proxyGrokManagement(proxyRequest, proxyRequest.Path, base)
}

func isGrokProxyRequest(path, base string) bool {
	relative := strings.TrimPrefix(path, base)
	return strings.HasPrefix(relative, "/api/grok2api/")
}

func proxyGrokManagement(req managementRequest, path, base string) ([]byte, error) {
	relative := strings.TrimPrefix(strings.TrimPrefix(path, base), "/api/grok2api")
	upstreamPath, ok := allowedGrokManagementPath(relative)
	if !ok {
		return managementJSONResponse(http.StatusNotFound, []byte(`{"error":{"code":"notFound","message":"not found"}}`))
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !allowedGrokManagementMethod(method, relative) {
		return managementJSONResponse(http.StatusMethodNotAllowed, []byte(`{"error":{"code":"methodNotAllowed","message":"method not allowed"}}`))
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return managementJSONResponse(http.StatusMethodNotAllowed, []byte(`{"error":{"code":"methodNotAllowed","message":"method not allowed"}}`))
	}
	if len(req.Body) > maxProxyRequestBody {
		return managementJSONResponse(http.StatusRequestEntityTooLarge, []byte(`{"error":{"code":"requestTooLarge","message":"request body is too large"}}`))
	}
	status, headers, body := doGrokManagementRequest(method, upstreamPath, req.Query, req.Body)
	return okEnvelope(managementResponse{
		StatusCode: status,
		Headers:    headers,
		Body:       body,
	})
}

func managementJSONResponse(status int, body []byte) ([]byte, error) {
	return okEnvelope(managementResponse{
		StatusCode: status,
		Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	})
}

func allowedGrokManagementPath(relative string) (string, bool) {
	clean := "/" + strings.TrimLeft(strings.TrimSpace(relative), "/")
	if strings.Contains(clean, "..") || strings.ContainsAny(clean, "?#") {
		return "", false
	}
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	switch {
	case clean == "/dashboard":
		return "/api/admin/v1/dashboard", true
	case clean == "/accounts", clean == "/accounts/summary", clean == "/accounts/export", clean == "/accounts/batch",
		clean == "/accounts/batch/refresh-quotas", clean == "/accounts/batch/refresh-tokens":
		return "/api/admin/v1" + clean, true
	case len(parts) == 2 && parts[0] == "accounts" && safePathID(parts[1]):
		return "/api/admin/v1/accounts/" + url.PathEscape(parts[1]), true
	case len(parts) == 3 && parts[0] == "accounts" && safePathID(parts[1]) && (parts[2] == "refresh-token" || parts[2] == "refresh-billing" || parts[2] == "refresh-quota"):
		return "/api/admin/v1/accounts/" + url.PathEscape(parts[1]) + "/" + parts[2], true
	case clean == "/models", clean == "/models/accounts", clean == "/models/sync", clean == "/models/batch":
		return "/api/admin/v1" + clean, true
	case len(parts) == 2 && parts[0] == "models" && safePathID(parts[1]):
		return "/api/admin/v1/models/" + url.PathEscape(parts[1]), true
	case clean == "/client-keys", clean == "/client-keys/batch":
		return "/api/admin/v1" + clean, true
	case len(parts) == 2 && parts[0] == "client-keys" && safePathID(parts[1]):
		return "/api/admin/v1/client-keys/" + url.PathEscape(parts[1]), true
	case len(parts) == 3 && parts[0] == "client-keys" && safePathID(parts[1]) && parts[2] == "secret":
		return "/api/admin/v1/client-keys/" + url.PathEscape(parts[1]) + "/secret", true
	case clean == "/request-audits", clean == "/request-audits/summary":
		return "/api/admin/v1" + clean, true
	case len(parts) == 2 && parts[0] == "request-audits" && safePathID(parts[1]):
		return "/api/admin/v1/request-audits/" + url.PathEscape(parts[1]), true
	case clean == "/settings":
		return "/api/admin/v1/settings", true
	case clean == "/quality-guard":
		return "/api/admin/v1/egress-quality-guard", true
	case clean == "/quality-guard/config":
		return "/api/admin/v1/egress-quality-guard/config", true
	case len(parts) == 4 && parts[0] == "quality-guard" && parts[1] == "nodes" && safePathID(parts[2]) && parts[3] == "test":
		return "/api/admin/v1/egress-quality-guard/nodes/" + url.PathEscape(parts[2]) + "/test", true
	case clean == "/nodes", clean == "/nodes/batch", clean == "/nodes/test":
		return "/api/admin/v1/egress-nodes" + strings.TrimPrefix(clean, "/nodes"), true
	case clean == "/nodes/accounts":
		return "/api/admin/v1/egress-nodes/accounts", true
	case len(parts) == 2 && parts[0] == "nodes" && safePathID(parts[1]):
		return "/api/admin/v1/egress-nodes/" + url.PathEscape(parts[1]), true
	case len(parts) == 3 && parts[0] == "nodes" && safePathID(parts[1]) && parts[2] == "accounts":
		return "/api/admin/v1/egress-nodes/" + url.PathEscape(parts[1]) + "/accounts", true
	case len(parts) == 3 && parts[0] == "nodes" && safePathID(parts[1]) && parts[2] == "test":
		return "/api/admin/v1/egress-nodes/" + url.PathEscape(parts[1]) + "/test", true
	case len(parts) == 3 && parts[0] == "nodes" && safePathID(parts[1]) && parts[2] == "quality-test":
		return "/api/admin/v1/egress-nodes/" + url.PathEscape(parts[1]) + "/quality-test", true
	default:
		return "", false
	}
}

func allowedGrokManagementMethod(method, relative string) bool {
	clean := "/" + strings.TrimLeft(strings.TrimSpace(relative), "/")
	switch clean {
	case "/accounts/export":
		return method == http.MethodPost
	case "/dashboard", "/accounts/summary", "/request-audits", "/request-audits/summary", "/quality-guard":
		return method == http.MethodGet
	case "/settings":
		return method == http.MethodGet || method == http.MethodPut
	case "/nodes/accounts":
		return method == http.MethodDelete
	default:
		return true
	}
}

func safePathID(value string) bool {
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

func doGrokManagementRequest(method, path string, query url.Values, body []byte) (int, http.Header, []byte) {
	cfg := loadConfig()
	requestURL := cfg.Grok2APIBaseURL + path
	if encoded := query.Encode(); encoded != "" {
		requestURL += "?" + encoded
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, errToken := grokAdmin.accessToken(cfg)
		if errToken != nil {
			return http.StatusServiceUnavailable, jsonResponseHeaders(), proxyErrorJSON("grok2apiAuthUnavailable", errToken.Error())
		}
		request, errRequest := http.NewRequest(method, requestURL, bytes.NewReader(body))
		if errRequest != nil {
			return http.StatusBadGateway, jsonResponseHeaders(), proxyErrorJSON("grok2apiRequestFailed", "无法创建 grok2api 请求")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "CPA-grok2api-egress/"+pluginVersion)
		if len(body) > 0 {
			request.Header.Set("Content-Type", "application/json")
		}
		timeout := 25 * time.Second
		if strings.HasSuffix(path, "/test") {
			timeout = 90 * time.Second
		}
		response, errDo := (&http.Client{Timeout: timeout}).Do(request)
		if errDo != nil {
			return http.StatusBadGateway, jsonResponseHeaders(), proxyErrorJSON("grok2apiUnavailable", "grok2api 请求失败")
		}
		responseBody, errRead := io.ReadAll(io.LimitReader(response.Body, maxProxyResponseBody+1))
		_ = response.Body.Close()
		if errRead != nil {
			return http.StatusBadGateway, jsonResponseHeaders(), proxyErrorJSON("grok2apiInvalidResponse", "读取 grok2api 响应失败")
		}
		if len(responseBody) > maxProxyResponseBody {
			return http.StatusBadGateway, jsonResponseHeaders(), proxyErrorJSON("grok2apiResponseTooLarge", "grok2api 响应过大")
		}
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 {
			grokAdmin.reset()
			continue
		}
		headers := jsonResponseHeaders()
		if contentType := response.Header.Get("Content-Type"); contentType != "" {
			headers.Set("Content-Type", contentType)
		}
		for _, name := range []string{"Content-Disposition", "Cache-Control", "Pragma", "X-Content-Type-Options", "X-Exported-Accounts"} {
			if value := response.Header.Get(name); value != "" {
				headers.Set(name, value)
			}
		}
		return response.StatusCode, headers, responseBody
	}
	return http.StatusUnauthorized, jsonResponseHeaders(), proxyErrorJSON("grok2apiAuthFailed", "grok2api 管理认证失败")
}

func jsonResponseHeaders() http.Header {
	return http.Header{"content-type": []string{"application/json; charset=utf-8"}}
}

func proxyErrorJSON(code, message string) []byte {
	raw, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return raw
}

func (s *adminSession) reset() {
	s.mu.Lock()
	s.token = ""
	s.expiresAt = time.Time{}
	s.mu.Unlock()
}

func (s *adminSession) accessToken(cfg pluginConfig) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Until(s.expiresAt) > 30*time.Second {
		return s.token, nil
	}
	username := strings.TrimSpace(os.Getenv("GROK2API_ADMIN_USERNAME"))
	password := os.Getenv("GROK2API_ADMIN_PASSWORD")
	if username == "" || password == "" {
		return "", fmt.Errorf("CPA 尚未配置 grok2api 管理凭证")
	}
	payload, _ := json.Marshal(map[string]string{"username": username, "password": password})
	request, errRequest := http.NewRequest(http.MethodPost, cfg.Grok2APIBaseURL+"/api/admin/v1/auth/login", bytes.NewReader(payload))
	if errRequest != nil {
		return "", fmt.Errorf("无法创建 grok2api 登录请求")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CPA-grok2api-egress/"+pluginVersion)
	response, errDo := (&http.Client{Timeout: 12 * time.Second}).Do(request)
	if errDo != nil {
		return "", fmt.Errorf("grok2api 管理登录不可用")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("grok2api 管理认证失败 (HTTP %d)", response.StatusCode)
	}
	var login struct {
		Data struct {
			Tokens struct {
				AccessToken          string `json:"accessToken"`
				AccessTokenExpiresAt string `json:"accessTokenExpiresAt"`
			} `json:"tokens"`
		} `json:"data"`
	}
	if errDecode := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&login); errDecode != nil || login.Data.Tokens.AccessToken == "" {
		return "", fmt.Errorf("grok2api 登录响应无效")
	}
	expiresAt, errExpiry := time.Parse(time.RFC3339, login.Data.Tokens.AccessTokenExpiresAt)
	if errExpiry != nil {
		expiresAt = time.Now().Add(10 * time.Minute)
	}
	s.token = login.Data.Tokens.AccessToken
	s.expiresAt = expiresAt
	return s.token, nil
}

func handleUsage(request []byte) ([]byte, error) {
	cfg := loadConfig()
	// Best-effort parse of common usage payload shapes.
	var payload map[string]any
	if len(request) > 0 {
		_ = json.Unmarshal(request, &payload)
	}
	ev := usageEvent{
		At: time.Now().UTC(),
	}
	ev.AuthID = firstString(payload, "auth_id", "authId", "AuthID", "credential_id", "id")
	ev.Model = firstString(payload, "model", "Model")
	ev.Provider = firstString(payload, "provider", "Provider", "type")
	ev.OutputTokens = firstInt(payload, "output_tokens", "outputTokens", "completion_tokens", "OutputTokens")
	ev.DurationMs = firstInt(payload, "duration_ms", "durationMs", "latency_ms", "total_ms")
	ev.FirstTokenMs = firstInt(payload, "first_token_ms", "firstTokenMs", "ttft_ms")
	if errText := firstString(payload, "error", "Error", "error_message"); errText != "" {
		ev.Error = truncate(errText, 160)
		ev.Class = "error"
	} else {
		ev.TPS = computeTPS(ev.OutputTokens, ev.DurationMs, ev.FirstTokenMs)
		ev.Class = classifyTPS(ev.TPS, cfg)
	}
	recordEvent(ev)
	if cfg.DisableOnHard && ev.Class == "hard" && ev.AuthID != "" {
		_ = tryDisableAuth(ev.AuthID)
	}
	atomic.AddInt64(&totalHandled, 1)
	return okEnvelope(map[string]any{
		"recorded": true,
		"class":    ev.Class,
		"tps":      ev.TPS,
	})
}

func computeTPS(outputTokens, durationMs, firstTokenMs int64) float64 {
	denom := durationMs - firstTokenMs
	if denom <= 0 || outputTokens <= 0 {
		return 0
	}
	return float64(outputTokens) / (float64(denom) / 1000.0)
}

func classifyTPS(tps float64, cfg pluginConfig) string {
	if tps <= 0 {
		return "unknown"
	}
	if tps >= cfg.HardTPS {
		return "hard"
	}
	if tps >= cfg.SoftTPS {
		return "soft"
	}
	return "healthy"
}

func recordEvent(ev usageEvent) {
	statsMu.Lock()
	defer statsMu.Unlock()
	recentEvents = append(recentEvents, ev)
	if len(recentEvents) > maxRecentEvents {
		recentEvents = recentEvents[len(recentEvents)-maxRecentEvents:]
	}
	key := ev.AuthID
	if key == "" {
		key = "(unknown)"
	}
	st := byAuth[key]
	if st == nil {
		st = &authStats{AuthID: key}
		byAuth[key] = st
	}
	st.Count++
	st.LastTPS = ev.TPS
	st.LastClass = ev.Class
	st.LastSeen = ev.At
	st.TotalOutTok += ev.OutputTokens
	st.TotalDurMs += ev.DurationMs
	switch ev.Class {
	case "hard":
		st.HardHits++
	case "soft":
		st.SoftHits++
	case "error":
		st.ErrorHits++
	}
}

func tryDisableAuth(authID string) error {
	// Fetch runtime auth, set disabled=true, save back. Best-effort; never fail usage path.
	result, errCall := callHost(pluginabi.MethodHostAuthGetRuntime, mustJSON(map[string]any{
		"name": authID,
	}))
	if errCall != nil {
		// try with filename style
		result, errCall = callHost(pluginabi.MethodHostAuthGet, mustJSON(map[string]any{
			"name": authID,
		}))
		if errCall != nil {
			return errCall
		}
	}
	var obj map[string]any
	if errUnmarshal := json.Unmarshal(result, &obj); errUnmarshal != nil {
		return errUnmarshal
	}
	// Host may wrap under "auth" / "json" / raw
	target := obj
	if inner, ok := obj["auth"].(map[string]any); ok {
		target = inner
	} else if raw, ok := obj["json"]; ok {
		switch t := raw.(type) {
		case map[string]any:
			target = t
		case string:
			_ = json.Unmarshal([]byte(t), &target)
		case json.RawMessage:
			_ = json.Unmarshal(t, &target)
		}
	}
	if target == nil {
		target = map[string]any{}
	}
	target["disabled"] = true
	target["disabled_reason"] = "grok2api-egress hard_tps"
	target["disabled_at"] = time.Now().UTC().Format(time.RFC3339)
	raw, errMarshal := json.Marshal(target)
	if errMarshal != nil {
		return errMarshal
	}
	_, errSave := callHost(pluginabi.MethodHostAuthSave, mustJSON(map[string]any{
		"name": authID,
		"json": json.RawMessage(raw),
	}))
	return errSave
}

type snapshot struct {
	Plugin           string         `json:"plugin"`
	Version          string         `json:"version"`
	StartedAt        string         `json:"started_at"`
	UptimeSec        int64          `json:"uptime_sec"`
	Config           pluginConfig   `json:"config"`
	TotalHandled     int64          `json:"total_handled"`
	AuthStats        []authStats    `json:"auth_stats"`
	Recent           []usageEvent   `json:"recent_events"`
	Grok2APIProbe    map[string]any `json:"grok2api_probe"`
	QualityGuardHint string         `json:"quality_guard_hint"`
}

func buildSnapshot() snapshot {
	cfg := loadConfig()
	statsMu.Lock()
	authList := make([]authStats, 0, len(byAuth))
	for _, st := range byAuth {
		authList = append(authList, *st)
	}
	recent := append([]usageEvent(nil), recentEvents...)
	statsMu.Unlock()
	sort.Slice(authList, func(i, j int) bool {
		if authList[i].HardHits == authList[j].HardHits {
			return authList[i].Count > authList[j].Count
		}
		return authList[i].HardHits > authList[j].HardHits
	})
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	return snapshot{
		Plugin:           pluginName,
		Version:          pluginVersion,
		StartedAt:        startedAt.Format(time.RFC3339),
		UptimeSec:        int64(time.Since(startedAt).Seconds()),
		Config:           cfg,
		TotalHandled:     atomic.LoadInt64(&totalHandled),
		AuthStats:        authList,
		Recent:           recent,
		Grok2APIProbe:    probeGrok2API(cfg),
		QualityGuardHint: "CPA-side passive TPS mirror of grok2api-egress-enhancements; hard/soft thresholds match quality-guard defaults.",
	}
}

func probeGrok2API(cfg pluginConfig) map[string]any {
	out := map[string]any{
		"base_url": cfg.Grok2APIBaseURL,
	}
	client := &http.Client{Timeout: time.Duration(cfg.FetchTimeoutSec) * time.Second}
	// Probe a few safe endpoints without credentials; report status codes only.
	paths := []string{"/health", "/api/health", "/v1/models", "/"}
	results := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		u := cfg.Grok2APIBaseURL + p
		start := time.Now()
		resp, err := client.Get(u)
		item := map[string]any{"path": p, "ms": time.Since(start).Milliseconds()}
		if err != nil {
			item["error"] = truncate(err.Error(), 120)
			results = append(results, item)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		_ = resp.Body.Close()
		item["status"] = resp.StatusCode
		item["body_prefix"] = truncate(string(body), 80)
		results = append(results, item)
		if resp.StatusCode > 0 && resp.StatusCode < 500 {
			out["reachable"] = true
		}
	}
	out["probes"] = results
	if _, ok := out["reachable"]; !ok {
		out["reachable"] = false
	}
	return out
}

func renderStatusPage() []byte {
	page := strings.Replace(pageTemplate, "/*__HALLMARK_TOKENS__*/", tokenCSS, 1)
	page = strings.Replace(page, "/*__APP_STYLES__*/", appCSS, 1)
	page = strings.Replace(page, "/*__APP_SCRIPT__*/", appJS, 1)
	return []byte(page)

	// Legacy server-rendered markup is intentionally retained below for one
	// release so existing plugin builds remain easy to compare and roll back.
	snap := buildSnapshot()
	hardHits, softHits, errorHits, healthyAuths := int64(0), int64(0), int64(0), 0
	for _, st := range snap.AuthStats {
		hardHits += st.HardHits
		softHits += st.SoftHits
		errorHits += st.ErrorHits
		if st.LastClass == "healthy" || st.LastClass == "" {
			healthyAuths++
		}
	}
	reachable, _ := snap.Grok2APIProbe["reachable"].(bool)
	serviceTone := "good"
	serviceValue := "运行中"
	if !reachable {
		serviceTone = "bad"
		serviceValue = "不可达"
	}
	quarantineTone := "good"
	if hardHits > 0 {
		quarantineTone = "bad"
	}

	var buf bytes.Buffer
	buf.WriteString(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<meta http-equiv="refresh" content="15"/>
<title>出口质量守护 · CPA</title>
<style>
:root {
  --radius: 0.5rem;
  --background: oklch(0.995 0 0);
  --foreground: oklch(0.145 0 0);
  --card: oklch(0.975 0.002 90);
  --card-foreground: oklch(0.145 0 0);
  --primary: oklch(0.12 0 0);
  --primary-foreground: oklch(0.985 0 0);
  --secondary: oklch(0.965 0.002 90);
  --secondary-foreground: oklch(0.2 0 0);
  --muted: oklch(0.965 0.002 90);
  --muted-foreground: oklch(0.55 0 0);
  --accent: oklch(0.95 0.003 90);
  --destructive: oklch(0.58 0.22 27);
  --border: oklch(0.91 0 0);
  --good: oklch(0.55 0.14 155);
  --warn: oklch(0.68 0.15 75);
  --shadow: 0 1px 2px oklch(0 0 0 / 0.04);
  color-scheme: light dark;
}
@media (prefers-color-scheme: dark) {
  :root {
    --background: oklch(0.13 0 0);
    --foreground: oklch(0.95 0 0);
    --card: oklch(0.18 0 0);
    --card-foreground: oklch(0.95 0 0);
    --primary: oklch(0.96 0 0);
    --primary-foreground: oklch(0.12 0 0);
    --secondary: oklch(0.21 0 0);
    --secondary-foreground: oklch(0.94 0 0);
    --muted: oklch(0.21 0 0);
    --muted-foreground: oklch(0.64 0 0);
    --accent: oklch(0.24 0 0);
    --destructive: oklch(0.65 0.2 28);
    --border: oklch(0.27 0 0);
    --good: oklch(0.72 0.13 155);
    --warn: oklch(0.8 0.13 80);
    --shadow: none;
  }
}
* { box-sizing: border-box; border-color: var(--border); }
html, body { margin: 0; padding: 0; background: var(--background); color: var(--foreground);
  font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, "PingFang SC", "Noto Sans SC", "Helvetica Neue", Arial, sans-serif;
  line-height: 1.45; -webkit-font-smoothing: antialiased; }
a { color: inherit; }
main { max-width: 1120px; margin: 0 auto; padding: 1.5rem 1.25rem 2.5rem; }
.space-y-6 > * + * { margin-top: 1.5rem; }
.page-header { display: flex; flex-wrap: wrap; align-items: flex-start; justify-content: space-between; gap: 1rem; }
.page-header h1 { margin: 0; font-size: 1.25rem; font-weight: 600; letter-spacing: -0.01em; }
.page-header p { margin: 0.35rem 0 0; font-size: 0.8125rem; color: var(--muted-foreground); max-width: 42rem; }
.btn { display: inline-flex; align-items: center; gap: 0.4rem; height: 2rem; padding: 0 0.75rem;
  border-radius: calc(var(--radius) - 2px); border: 1px solid var(--border); background: var(--secondary);
  color: var(--secondary-foreground); font-size: 0.8125rem; font-weight: 500; cursor: default; text-decoration: none; }
.btn-ghost { background: transparent; }
.btn svg { width: 14px; height: 14px; }
.metrics { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); overflow: hidden; border-radius: var(--radius); background: var(--card); box-shadow: var(--shadow); }
@media (max-width: 900px) { .metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 520px) { .metrics { grid-template-columns: 1fr; } }
.metric { display: flex; gap: 0.75rem; padding: 1rem 1.1rem; border-right: 1px solid var(--border); border-bottom: 1px solid var(--border); min-height: 5.5rem; }
.metrics .metric:nth-child(4n) { border-right: none; }
@media (max-width: 900px) { .metrics .metric:nth-child(2n) { border-right: none; } .metrics .metric:nth-child(4n) { border-right: 1px solid var(--border); } .metrics .metric:nth-child(2n) { border-right: none; } }
.metric-icon { display: flex; align-items: center; justify-content: center; width: 2rem; height: 2rem; border-radius: 0.375rem; background: var(--secondary); color: var(--muted-foreground); flex-shrink: 0; }
.metric-icon svg { width: 1rem; height: 1rem; }
.metric-icon.good { color: var(--good); background: color-mix(in oklch, var(--good) 12%, transparent); }
.metric-icon.bad { color: var(--destructive); background: color-mix(in oklch, var(--destructive) 12%, transparent); }
.metric-icon.warn { color: var(--warn); background: color-mix(in oklch, var(--warn) 14%, transparent); }
.metric-label { margin: 0; font-size: 0.75rem; color: var(--muted-foreground); }
.metric-value { margin: 0.2rem 0 0; font-size: 1.125rem; font-weight: 550; font-variant-numeric: tabular-nums; letter-spacing: -0.01em; }
.metric-value.good { color: var(--good); }
.metric-value.bad { color: var(--destructive); }
.metric-value.warn { color: var(--warn); }
.card { overflow: hidden; border-radius: var(--radius); background: var(--card); box-shadow: var(--shadow); }
.card-head { display: flex; flex-wrap: wrap; align-items: flex-start; justify-content: space-between; gap: 0.75rem; padding: 1rem 1.25rem; border-bottom: 1px solid var(--border); }
.card-head h2 { margin: 0; font-size: 0.875rem; font-weight: 550; }
.card-head p { margin: 0.3rem 0 0; font-size: 0.75rem; color: var(--muted-foreground); }
.card-body { padding: 0; }
.stats-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); border-top: 1px solid var(--border); }
@media (max-width: 900px) { .stats-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 520px) { .stats-grid { grid-template-columns: 1fr; } }
.stat { display: flex; gap: 0.75rem; min-height: 6rem; padding: 1rem; border-right: 1px solid var(--border); border-bottom: 1px solid var(--border); }
.stats-grid .stat:nth-child(3n) { border-right: none; }
.stat-icon { display: flex; align-items: center; justify-content: center; width: 2rem; height: 2rem; border-radius: 0.375rem; background: var(--secondary); color: var(--muted-foreground); flex-shrink: 0; }
.stat-icon svg { width: 1rem; height: 1rem; }
.stat-label { margin: 0; font-size: 0.75rem; color: var(--muted-foreground); }
.stat-value { margin: 0.25rem 0 0; font-size: 1.125rem; font-weight: 550; font-variant-numeric: tabular-nums; }
.stat-detail { margin: 0.25rem 0 0; font-size: 0.6875rem; color: var(--muted-foreground); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.table-wrap { overflow-x: auto; }
table { width: 100%; border-collapse: collapse; min-width: 720px; }
th { text-align: left; font-size: 0.75rem; font-weight: 500; color: var(--muted-foreground); padding: 0.7rem 1rem; border-bottom: 1px solid var(--border); background: transparent; white-space: nowrap; }
td { padding: 0.8rem 1rem; border-bottom: 1px solid var(--border); font-size: 0.8125rem; vertical-align: middle; }
tr:last-child td { border-bottom: none; }
tr:hover td { background: color-mix(in oklch, var(--secondary) 55%, transparent); }
.right { text-align: right; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-variant-numeric: tabular-nums; font-size: 0.75rem; }
.muted { color: var(--muted-foreground); }
.cell-title { font-weight: 550; }
.cell-sub { margin-top: 0.15rem; font-size: 0.6875rem; color: var(--muted-foreground); }
.badge { display: inline-flex; align-items: center; height: 1.35rem; padding: 0 0.5rem; border-radius: 999px; font-size: 0.6875rem; font-weight: 550; }
.badge-healthy { background: color-mix(in oklch, var(--good) 14%, transparent); color: var(--good); }
.badge-soft { background: color-mix(in oklch, var(--warn) 16%, transparent); color: var(--warn); }
.badge-hard, .badge-error { background: color-mix(in oklch, var(--destructive) 14%, transparent); color: var(--destructive); }
.badge-unknown, .badge-secondary { background: var(--secondary); color: var(--muted-foreground); }
.tps-hard { color: var(--destructive); font-weight: 550; }
.tps-soft { color: var(--warn); }
.split { display: grid; grid-template-columns: minmax(0, 3fr) minmax(280px, 2fr); gap: 0.75rem; }
@media (max-width: 900px) { .split { grid-template-columns: 1fr; } }
.event { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 1rem; padding: 0.55rem 0.6rem; border-radius: 0.375rem; }
.event:hover { background: color-mix(in oklch, var(--secondary) 55%, transparent); }
.event-title { margin: 0; font-size: 0.75rem; font-weight: 550; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.event-sub { margin: 0.2rem 0 0; font-size: 0.6875rem; color: var(--muted-foreground); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.event-time { font-size: 0.6875rem; color: var(--muted-foreground); white-space: nowrap; }
.policy { padding: 1rem 1.25rem; }
.policy-head { display: flex; align-items: center; gap: 0.5rem; }
.policy-head h2 { margin: 0; font-size: 0.875rem; font-weight: 550; }
.policy dl { margin: 1rem 0 0; display: grid; grid-template-columns: 1fr 1fr; gap: 1rem 1.25rem; }
.policy dt { margin: 0; font-size: 0.6875rem; color: var(--muted-foreground); }
.policy dd { margin: 0.25rem 0 0; font-size: 0.875rem; font-weight: 550; font-variant-numeric: tabular-nums; }
.empty { padding: 2.5rem 1rem; text-align: center; font-size: 0.75rem; color: var(--muted-foreground); }
.footer-note { margin-top: 0.25rem; font-size: 0.75rem; color: var(--muted-foreground); }
.footer-note code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 0.7rem; background: var(--secondary); padding: 0.1rem 0.35rem; border-radius: 0.25rem; }
</style>
</head>
<body>
<main class="space-y-6">
`)

	// header
	buf.WriteString(`<div class="page-header"><div>`)
	buf.WriteString(`<h1>出口质量守护</h1>`)
	buf.WriteString(`<p>CPA 插件视图 · 固定代理快速恢复 / 被动 TPS 熔断 · 与 grok2api Quality Guard 同口径（outputTokens / (duration − TTFT)）</p>`)
	buf.WriteString(`</div>`)
	buf.WriteString(`<a class="btn" href="?">`)
	buf.WriteString(svgRefresh())
	buf.WriteString(`刷新</a></div>`)

	// metrics
	buf.WriteString(`<section class="metrics" aria-label="总览">`)
	writeMetric(&buf, svgShield(), "服务状态", serviceValue, serviceTone)
	writeMetric(&buf, svgActivity(), "检测模式", "hybrid / CPA 被动", "")
	writeMetric(&buf, svgGauge(), "已跟踪凭证", fmt.Sprintf("%d / %d", healthyAuths, len(snap.AuthStats)), "")
	writeMetric(&buf, svgTimer(), "Hard 命中", fmt.Sprintf("%d", hardHits), quarantineTone)
	buf.WriteString(`</section>`)

	// statistics
	buf.WriteString(`<section class="card" aria-labelledby="stats-title">`)
	buf.WriteString(`<div class="card-head"><div><h2 id="stats-title">累计统计</h2>`)
	buf.WriteString(fmt.Sprintf(`<p>自插件启动 · %s · 运行 %s</p>`, html.EscapeString(snap.StartedAt), html.EscapeString(formatUptime(snap.UptimeSec))))
	buf.WriteString(`</div></div><div class="stats-grid">`)
	writeStat(&buf, svgBar(), "处理次数", fmt.Sprintf("%d", snap.TotalHandled), "usage.handle 被动审计")
	writeStat(&buf, svgEye(), "Soft 命中", fmt.Sprintf("%d", softHits), fmt.Sprintf("阈值 ≥ %.0f Token/s", snap.Config.SoftTPS))
	writeStat(&buf, svgAlert(), "Hard / Error", fmt.Sprintf("%d / %d", hardHits, errorHits), fmt.Sprintf("阈值 ≥ %.0f Token/s", snap.Config.HardTPS))
	writeStat(&buf, svgBot(), "凭证数", fmt.Sprintf("%d", len(snap.AuthStats)), "按 auth_id 聚合")
	writeStat(&buf, svgCoins(), "最近事件", fmt.Sprintf("%d", len(snap.Recent)), "最多保留 40 条")
	g2a := "不可达"
	if reachable {
		g2a = "可达"
	}
	writeStat(&buf, svgZap(), "grok2api", g2a, html.EscapeString(snap.Config.Grok2APIBaseURL))
	buf.WriteString(`</div></section>`)

	// auth table
	buf.WriteString(`<section class="card" aria-labelledby="nodes-title">`)
	buf.WriteString(`<div class="card-head"><div><h2 id="nodes-title">凭证质量</h2>`)
	buf.WriteString(`<p>CPA 侧被动观察 · Hard 立即标记 · Soft 连续命中后关注</p></div>`)
	buf.WriteString(fmt.Sprintf(`<span class="muted" style="font-size:0.75rem">自动刷新 · %ds</span>`, 15))
	buf.WriteString(`</div>`)
	if len(snap.AuthStats) == 0 {
		buf.WriteString(`<div class="empty">暂无 usage 事件。有请求经过 CPA 后会在此展示凭证 TPS 与熔断状态。</div>`)
	} else {
		buf.WriteString(`<div class="table-wrap"><table><thead><tr>`)
		for _, h := range []string{"凭证", "状态", "输出 TPS", "次数", "Hard / Soft / Error", "最近观察"} {
			align := ""
			if h == "输出 TPS" || h == "次数" {
				align = ` class="right"`
			}
			buf.WriteString(fmt.Sprintf("<th%s>%s</th>", align, h))
		}
		buf.WriteString(`</tr></thead><tbody>`)
		for _, st := range snap.AuthStats {
			class := st.LastClass
			if class == "" {
				class = "unknown"
			}
			tpsClass := ""
			switch class {
			case "hard":
				tpsClass = "tps-hard"
			case "soft":
				tpsClass = "tps-soft"
			}
			buf.WriteString(`<tr>`)
			buf.WriteString(`<td><div class="cell-title">`)
			buf.WriteString(html.EscapeString(shortAuth(st.AuthID)))
			buf.WriteString(`</div><div class="cell-sub">`)
			buf.WriteString(html.EscapeString(st.AuthID))
			buf.WriteString(`</div></td><td>`)
			writeStateBadge(&buf, class)
			buf.WriteString(`</td><td class="right mono `)
			buf.WriteString(tpsClass)
			buf.WriteString(`">`)
			if st.LastTPS > 0 {
				buf.WriteString(html.EscapeString(fmt.Sprintf("%.1f Token/s", st.LastTPS)))
			} else {
				buf.WriteString(`-`)
			}
			buf.WriteString(`</td><td class="right mono">`)
			buf.WriteString(fmt.Sprintf("%d", st.Count))
			buf.WriteString(`</td><td class="mono">`)
			buf.WriteString(fmt.Sprintf("%d / %d / %d", st.HardHits, st.SoftHits, st.ErrorHits))
			buf.WriteString(`</td><td class="muted" style="font-size:0.75rem">`)
			if st.LastSeen.IsZero() {
				buf.WriteString(`-`)
			} else {
				buf.WriteString(html.EscapeString(st.LastSeen.Local().Format("01-02 15:04:05")))
			}
			buf.WriteString(`</td></tr>`)
		}
		buf.WriteString(`</tbody></table></div>`)
	}
	buf.WriteString(`</section>`)

	// split: events + policy
	buf.WriteString(`<div class="split">`)
	// events
	buf.WriteString(`<section class="card"><div class="card-head"><div><h2>最近事件</h2><p>被动审计时间线</p></div></div>`)
	if len(snap.Recent) == 0 {
		buf.WriteString(`<div class="empty">暂无事件</div>`)
	} else {
		buf.WriteString(`<div style="padding:0.5rem 0.75rem 1rem">`)
		limit := len(snap.Recent)
		if limit > 12 {
			limit = 12
		}
		for i := 0; i < limit; i++ {
			ev := snap.Recent[i]
			title := shortAuth(ev.AuthID)
			if title == "" || title == "(unknown)" {
				title = "unknown"
			}
			if ev.Model != "" {
				title = title + " · " + ev.Model
			}
			sub := classLabel(ev.Class)
			if ev.TPS > 0 {
				sub = fmt.Sprintf("%s · %.1f Token/s", sub, ev.TPS)
			}
			if ev.Error != "" {
				sub = sub + " · " + truncate(ev.Error, 60)
			}
			buf.WriteString(`<div class="event"><div><p class="event-title">`)
			buf.WriteString(html.EscapeString(title))
			buf.WriteString(`</p><p class="event-sub">`)
			buf.WriteString(html.EscapeString(sub))
			buf.WriteString(`</p></div><time class="event-time">`)
			buf.WriteString(html.EscapeString(ev.At.Local().Format("15:04:05")))
			buf.WriteString(`</time></div>`)
		}
		buf.WriteString(`</div>`)
	}
	buf.WriteString(`</section>`)

	// policy
	buf.WriteString(`<section class="card policy"><div class="policy-head">`)
	buf.WriteString(svgZap())
	buf.WriteString(`<h2>策略</h2></div><dl>`)
	writePolicy(&buf, "软阈值", fmt.Sprintf("%.0f Token/s", snap.Config.SoftTPS))
	writePolicy(&buf, "硬阈值", fmt.Sprintf("%.0f Token/s", snap.Config.HardTPS))
	writePolicy(&buf, "Hard 自动禁用", boolCN(snap.Config.DisableOnHard))
	writePolicy(&buf, "探测超时", fmt.Sprintf("%ds", snap.Config.FetchTimeoutSec))
	writePolicy(&buf, "插件版本", snap.Version)
	writePolicy(&buf, "grok2api", snap.Config.Grok2APIBaseURL)
	buf.WriteString(`</dl></section></div>`)

	buf.WriteString(`<p class="footer-note">源项目 <code>grok2api-egress-enhancements</code> · 本页为 CPA 管理资源 · 与面板 <code>Quality Guard</code> 视觉对齐 · JSON：同路径 <code>/api/snapshot</code></p>`)
	buf.WriteString(`</main></body></html>`)
	return buf.Bytes()
}

func writeMetric(buf *bytes.Buffer, icon, label, value, tone string) {
	buf.WriteString(`<div class="metric"><span class="metric-icon`)
	if tone != "" {
		buf.WriteString(" ")
		buf.WriteString(html.EscapeString(tone))
	}
	buf.WriteString(`">`)
	buf.WriteString(icon)
	buf.WriteString(`</span><div><p class="metric-label">`)
	buf.WriteString(html.EscapeString(label))
	buf.WriteString(`</p><p class="metric-value`)
	if tone != "" {
		buf.WriteString(" ")
		buf.WriteString(html.EscapeString(tone))
	}
	buf.WriteString(`">`)
	buf.WriteString(html.EscapeString(value))
	buf.WriteString(`</p></div></div>`)
}

func writeStat(buf *bytes.Buffer, icon, label, value, detail string) {
	buf.WriteString(`<div class="stat"><span class="stat-icon">`)
	buf.WriteString(icon)
	buf.WriteString(`</span><div style="min-width:0"><p class="stat-label">`)
	buf.WriteString(html.EscapeString(label))
	buf.WriteString(`</p><p class="stat-value">`)
	buf.WriteString(html.EscapeString(value))
	buf.WriteString(`</p><p class="stat-detail" title="`)
	buf.WriteString(html.EscapeString(detail))
	buf.WriteString(`">`)
	buf.WriteString(detail) // already escaped by callers when needed; plain text ok
	buf.WriteString(`</p></div></div>`)
}

func writePolicy(buf *bytes.Buffer, label, value string) {
	buf.WriteString(`<div><dt>`)
	buf.WriteString(html.EscapeString(label))
	buf.WriteString(`</dt><dd>`)
	buf.WriteString(html.EscapeString(value))
	buf.WriteString(`</dd></div>`)
}

func writeStateBadge(buf *bytes.Buffer, class string) {
	if class == "" {
		class = "unknown"
	}
	buf.WriteString(`<span class="badge badge-`)
	buf.WriteString(html.EscapeString(class))
	buf.WriteString(`">`)
	buf.WriteString(html.EscapeString(classLabel(class)))
	buf.WriteString(`</span>`)
}

func classLabel(class string) string {
	switch class {
	case "healthy":
		return "健康"
	case "soft":
		return "软阈值"
	case "hard":
		return "硬阈值"
	case "error":
		return "错误"
	case "unknown":
		return "未知"
	default:
		return class
	}
}

func shortAuth(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "(unknown)"
	}
	// xai-email.json or email
	base := id
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".json")
	base = strings.TrimPrefix(base, "xai-")
	if len(base) > 36 {
		return base[:18] + "…" + base[len(base)-10:]
	}
	return base
}

func formatUptime(sec int64) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
}

func boolCN(v bool) string {
	if v {
		return "开启"
	}
	return "关闭"
}

func svgRefresh() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-9-9c2.5 0 4.8 1 6.5 2.7L21 8"/><path d="M21 3v5h-5"/></svg>`
}
func svgShield() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10"/><path d="m9 12 2 2 4-4"/></svg>`
}
func svgActivity() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg>`
}
func svgGauge() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m12 14 4-4"/><path d="M3.34 19a10 10 0 1 1 17.32 0"/></svg>`
}
func svgTimer() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10 2h4"/><path d="M12 14v-4"/><circle cx="12" cy="14" r="8"/></svg>`
}
func svgBar() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3v16a2 2 0 0 0 2 2h16"/><path d="M7 16v-5"/><path d="M12 16V8"/><path d="M17 16v-2"/></svg>`
}
func svgEye() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2.06 12.35a1 1 0 0 1 0-.7C3.8 7.7 7.7 4.6 12 4.6s8.2 3.1 9.94 7.05a1 1 0 0 1 0 .7C20.2 16.3 16.3 19.4 12 19.4s-8.2-3.1-9.94-7.05"/><circle cx="12" cy="12" r="3"/></svg>`
}
func svgAlert() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/></svg>`
}
func svgBot() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 8V4H8"/><rect width="16" height="12" x="4" y="8" rx="2"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/></svg>`
}
func svgCoins() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="8" cy="8" r="6"/><path d="M18.09 10.37A6 6 0 1 1 10.34 18"/><path d="M7 6h1v4"/><path d="m16.71 13.88.7.71-2.82 2.82"/></svg>`
}
func svgZap() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="16" height="16"><path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z"/></svg>`
}

func firstString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				if s := strings.TrimSpace(t); s != "" {
					return s
				}
			case fmt.Stringer:
				if s := strings.TrimSpace(t.String()); s != "" {
					return s
				}
			}
		}
	}
	// nested usage / request common wrappers
	for _, wrap := range []string{"usage", "meta", "request", "data"} {
		if nested, ok := m[wrap].(map[string]any); ok {
			if s := firstString(nested, keys...); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
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
			case string:
				var n int64
				fmt.Sscan(t, &n)
				return n
			}
		}
	}
	for _, wrap := range []string{"usage", "metrics", "request", "data"} {
		if nested, ok := m[wrap].(map[string]any); ok {
			if n := firstInt(nested, keys...); n != 0 {
				return n
			}
		}
	}
	return 0
}

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
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
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
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
