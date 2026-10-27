package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*host_free_fn)(void*, size_t);
typedef struct {
    uint32_t abi_version;
    void* host_ctx;
    host_call_fn call;
    host_free_fn free_buffer;
} cliproxy_host_api;
typedef int (*plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*plugin_free_fn)(void*, size_t);
typedef void (*plugin_shutdown_fn)(void);
typedef struct {
    uint32_t abi_version;
    plugin_call_fn call;
    plugin_free_fn free_buffer;
    plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int searchPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void searchPluginFree(void*, size_t);
extern void searchPluginShutdown(void);

static cliproxy_host_api search_host;
static void install_host(cliproxy_host_api* host, cliproxy_plugin_api* plugin) {
    search_host = *host;
    plugin->abi_version = 1;
    plugin->call = (plugin_call_fn)searchPluginCall;
    plugin->free_buffer = (plugin_free_fn)searchPluginFree;
    plugin->shutdown = (plugin_shutdown_fn)searchPluginShutdown;
}
static int invoke_host(const char* method, const uint8_t* data, size_t len, cliproxy_buffer* out) {
    if (!search_host.call) return 1;
    return search_host.call(search_host.host_ctx, method, data, len, out);
}
static void release_host(cliproxy_buffer* out) {
    if (out->ptr && search_host.free_buffer) search_host.free_buffer(out->ptr, out->len);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"unsafe"
)

var pluginState = struct {
	sync.RWMutex
	engine *Engine
}{}

func nativeHostCall(method string, data any, result any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	cData := C.CBytes(raw)
	defer C.free(cData)
	var out C.cliproxy_buffer
	code := C.invoke_host(cMethod, (*C.uint8_t)(cData), C.size_t(len(raw)), &out)
	defer C.release_host(&out)
	if out.ptr == nil {
		return fmt.Errorf("host callback %s failed (%d)", method, int(code))
	}
	var envelope struct {
		OK     bool
		Result json.RawMessage
		Error  *RPCError
	}
	if err := json.Unmarshal(C.GoBytes(out.ptr, C.int(out.len)), &envelope); err != nil {
		return fmt.Errorf("invalid host callback envelope for %s", method)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return envelope.Error
		}
		return fmt.Errorf("host callback %s failed", method)
	}
	if result != nil && len(envelope.Result) > 0 {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.abi_version != 1 {
		return 1
	}
	C.install_host(host, plugin)
	return 0
}

//export searchPluginCall
func searchPluginCall(method *C.char, data *C.uint8_t, length C.size_t, out *C.cliproxy_buffer) C.int {
	if out == nil {
		return 1
	}
	out.ptr, out.len = nil, 0
	if method == nil {
		return 1
	}
	raw := C.GoBytes(unsafe.Pointer(data), C.int(length))
	result, err := dispatch(C.GoString(method), raw)
	var payload []byte
	if err != nil {
		status := 500
		if e, ok := err.(*RPCError); ok && e.HTTPStatus != 0 {
			status = e.HTTPStatus
		}
		payload, _ = json.Marshal(map[string]any{"ok": false, "error": RPCError{
			Code: "deepseek_search_error", Message: safeError(err), HTTPStatus: status,
		}})
	} else {
		payload, _ = json.Marshal(map[string]any{"ok": true, "result": result})
	}
	out.ptr = C.CBytes(payload)
	out.len = C.size_t(len(payload))
	return 0
}

//export searchPluginFree
func searchPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	C.free(ptr)
}

//export searchPluginShutdown
func searchPluginShutdown() {
	pluginState.Lock()
	if pluginState.engine != nil {
		pluginState.engine.Close()
	}
	pluginState.Unlock()
}

func dispatch(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var request struct {
			ConfigYAML    []byte `json:"config_yaml"`
			SchemaVersion int    `json:"schema_version"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		cfg, err := loadConfig(request.ConfigYAML)
		if err != nil {
			return nil, err
		}
		engine, err := NewEngine(cfg, nativeHostCall)
		if err != nil {
			return nil, err
		}
		pluginState.Lock()
		if pluginState.engine != nil {
			if reflect.DeepEqual(pluginState.engine.cfg, engine.cfg) && pluginState.engine.key == engine.key {
				// Host startup may reconfigure unchanged settings while streams run.
				engine.Close()
				engine = pluginState.engine
			} else {
				pluginState.engine.Close()
			}
		}
		pluginState.engine = engine
		pluginState.Unlock()
		return map[string]any{
			"schema_version": 3,
			"metadata": map[string]any{
				"Name": "deepseek-search", "Version": "1.0.2", "Author": "duckkkkkkkkking",
				"GitHubRepository": "https://github.com/duckkkkkkkkking/deepseek_search_fetch_plugin",
				"Logo":             "https://raw.githubusercontent.com/duckkkkkkkkking/deepseek_search_fetch_plugin/main/assets/logo.svg",
				"ConfigFields": []any{map[string]any{
					"Name": "settings_file", "Type": "string",
					"Description": "Absolute path to private plugin JSON settings.",
				}},
			},
			"capabilities": map[string]any{
				"model_router": true, "executor": true, "executor_model_scope": "both",
				"executor_input_formats":  []string{"openai-response", "openai"},
				"executor_output_formats": []string{"openai-response", "openai"},
			},
		}, nil
	case "plugin.quiesce", "plugin.shutdown":
		searchPluginShutdown()
		return map[string]any{}, nil
	case "executor.identifier":
		return map[string]string{"identifier": "deepseek-search"}, nil
	}
	pluginState.RLock()
	engine := pluginState.engine
	pluginState.RUnlock()
	if engine == nil {
		return nil, fmt.Errorf("plugin is not configured")
	}
	switch method {
	case "model.route":
		var request struct {
			RequestedModel string
			SourceFormat   string
			Body           []byte
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return map[string]any{
			"Handled":    engine.ShouldHandle(request.RequestedModel, request.SourceFormat, request.Body),
			"TargetKind": "self", "Reason": "isolated server-side DeepSeek search/fetch",
		}, nil
	case "executor.execute", "executor.execute_stream":
		var request Execution
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		if method == "executor.execute_stream" {
			if request.StreamID == "" {
				return nil, fmt.Errorf("missing host stream ID")
			}
			go engine.ExecuteStream(request)
			return map[string]any{"headers": map[string][]string{
				"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"},
			}}, nil
		}
		result, err := engine.Execute(request, nil)
		if err != nil {
			return nil, err
		}
		bytes, _ := json.Marshal(result)
		return map[string]any{"Payload": bytes, "Headers": map[string][]string{
			"Content-Type": {"application/json"},
		}}, nil
	case "executor.count_tokens":
		return nil, &RPCError{Message: "token counting is not implemented by the isolated plugin", HTTPStatus: 501}
	default:
		return nil, fmt.Errorf("unsupported plugin method %s", method)
	}
}

func main() {}
