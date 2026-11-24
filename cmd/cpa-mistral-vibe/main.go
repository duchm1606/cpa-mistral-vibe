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
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"unsafe"
)

const abiVersion uint32 = 1

var (
	stateMu sync.Mutex
	state   = defaultConfig()
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = (C.cliproxy_plugin_call_fn)(C.cliproxyPluginCall)
	plugin.free_buffer = (C.cliproxy_plugin_free_fn)(C.cliproxyPluginFree)
	plugin.shutdown = (C.cliproxy_plugin_shutdown_fn)(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeResponse(response, handleMethod(C.GoString(method), requestBytes))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

const maxSupportedSchema uint32 = 3

func negotiateSchema(host uint32) uint32 {
	if host == 0 {
		return 1
	}
	if host < maxSupportedSchema {
		return host
	}
	return maxSupportedSchema
}

const providerKey = "mistral"

func handleMethod(method string, request []byte) []byte {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var life lifecycleRequest
		_ = json.Unmarshal(request, &life)
		storeConfig(parseConfig(life.ConfigYAML))
		return registrationJSON(negotiateSchema(life.SchemaVersion))

	// --- auth provider (Vibe OAuth login + credential parsing) ---
	case "auth.identifier":
		return mustOK(map[string]any{"identifier": providerKey})
	case "auth.parse":
		return handleAuthParse(request)
	case "auth.login.start":
		return handleLoginStart(request)
	case "auth.login.poll":
		return handleLoginPoll(request)
	case "auth.refresh":
		return handleAuthRefresh(request)

	// --- quota provider (Vibe Code + API allowance) ---
	case "quota.identifier":
		return mustOK(map[string]any{"identifier": providerKey})
	case "quota.describe":
		return mustOK(map[string]any{
			"supported_providers": []string{providerKey},
			"display_name":        "Mistral",
			"supports_reset":      false,
		})
	case "quota.fetch":
		return handleQuotaFetch(request)
	case "quota.reset":
		return mustOK(map[string]any{"success": false, "message": "Mistral allowance resets on the 1st of each month and cannot be reset manually."})

	// --- model registration (inference runs on the host's built-in
	//     OpenAI-compat executor via the auth's base_url + api_key attributes) ---
	case "model.register", "model.static":
		return modelsJSON(configuredKey())
	case "model.for_auth":
		return modelsJSON(authModelKey(request))

	// --- response normalization: flatten Mistral's thinking-chunk content
	//     into content + reasoning_content (ported from cpa-mistral-normalizer) ---
	case "response.normalize_before":
		return handleNormalizeBefore(request)

	// --- request fixes (scoped to our Mistral models): developer->system,
	//     reasoning_effort medium->high, drop assistant reasoning_content ---
	case "request.intercept_before":
		return handleInterceptBefore(request)
	case "request.intercept_after":
		return handleInterceptAfter(request)
	// Same fixes on the translated provider payload — the only point where a
	// Responses-API (Codex) request has become chat messages[].
	case "request.normalize":
		return handleRequestNormalize(request)

	// --- management page intentionally hidden: the Vibe Code / API allowance
	//     now appears in the native Quota Management page via quota_provider,
	//     so no separate sidebar menu is registered. ---
	case "management.register":
		return mustOK(map[string]any{"resources": []map[string]string{}})
	case "management.handle":
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0)
	}
}

func registrationJSON(schemaVersion uint32) []byte {
	return mustOK(map[string]any{
		"schema_version": schemaVersion,
		"metadata": map[string]any{
			"Name":             "Mistral Vibe",
			"Version":          pluginVersion,
			"Author":           "duchm1606",
			"GitHubRepository": "https://github.com/duchm1606/cpa-mistral-vibe",
			"Logo":             logoDataURI,
			"ConfigFields": []map[string]string{
				{"Name": "api-key", "Type": "string", "Description": "Mistral key (mstrl_...). Leave empty to auto-detect from the CPA config file."},
				{"Name": "config-path", "Type": "string", "Description": "Path to the CPA config file to read the Mistral key from. Default /app/config.yaml."},
				{"Name": "strip-excess-images", "Type": "boolean", "Description": "Replace the oldest images with a text note when a request carries more than max-images. Off by default."},
				{"Name": "max-images", "Type": "integer", "Description": "Image limit used by strip-excess-images. Default 20 (Mistral's limit)."},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":              true,
			"quota_provider":             true,
			"management_api":             true,
			"model_registrar":            true,
			"model_provider":             true,
			"response_before_translator": true,
			"request_interceptor":        true,
			"request_normalizer":         true,
		},
	})
}

func handleManagement(request []byte) []byte {
	var req struct {
		Method string `json:"Method"`
		Path   string `json:"Path"`
		Body   []byte `json:"Body"`
	}
	_ = json.Unmarshal(request, &req)

	html := renderPage()
	return mustOK(map[string]any{
		"StatusCode": http.StatusOK,
		"Headers":    map[string][]string{"content-type": {"text/html; charset=utf-8"}},
		"Body":       []byte(html),
	})
}

// ---- envelope plumbing ----

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func mustOK(value any) []byte {
	result, err := json.Marshal(value)
	if err != nil {
		return errorEnvelope("plugin_error", err.Error(), 0)
	}
	raw, err := json.Marshal(envelope{OK: true, Result: result})
	if err != nil {
		return errorEnvelope("plugin_error", err.Error(), 0)
	}
	return raw
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus}})
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

// callHost is kept for completeness; this plugin uses direct net/http instead.
func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(rawPayload) > 0 {
		req = (*C.uint8_t)(C.CBytes(rawPayload))
		defer C.free(unsafe.Pointer(req))
	}
	status := C.call_host_api(cMethod, req, C.size_t(len(rawPayload)), &response)
	if response.ptr == nil {
		if status != 0 {
			return nil, fmt.Errorf("host callback %s failed", method)
		}
		return json.RawMessage(`{}`), nil
	}
	defer C.free_host_buffer(response.ptr, response.len)
	body := C.GoBytes(unsafe.Pointer(response.ptr), C.int(response.len))
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if !env.OK {
		message := method + " failed"
		if env.Error != nil && env.Error.Message != "" {
			message = env.Error.Message
		}
		return nil, fmt.Errorf("%s", message)
	}
	if len(env.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return env.Result, nil
}
