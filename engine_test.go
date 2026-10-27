package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func copyInto(value any, target any) error {
	if target == nil {
		return nil
	}
	raw, _ := json.Marshal(value)
	return json.Unmarshal(raw, target)
}

func testEngine(t *testing.T, call HostCall) *Engine {
	t.Helper()
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("DUMMY_PRIVATE_TEST_KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{KeyFile: key, APIBase: "http://127.0.0.1:1",
		AllowTestAPI: true, Models: []string{"test-model"}}, call)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func execution(body Object) Execution {
	raw, _ := json.Marshal(body)
	return Execution{Model: "test-model", Payload: raw, CallbackID: "test-callback",
		Headers: map[string][]string{"Authorization": {"Bearer TEST_CLIENT_A"}}}
}

func toolCall(name, id string, arguments Object) Object {
	raw, _ := json.Marshal(arguments)
	return Object{"id": "fc_" + id, "type": "function_call", "call_id": id,
		"name": name, "arguments": string(raw), "status": "completed"}
}

func response(output ...any) Object {
	return Object{"id": "resp_upstream", "object": "response", "status": "completed", "output": output,
		"usage": Object{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
}

func textItem(text string) Object {
	return Object{"id": "msg_upstream", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{Object{"type": "output_text", "text": text}}}
}

func modelReturn(value Object, target any) error {
	raw, _ := json.Marshal(value)
	return copyInto(Object{"status_code": 200, "body": raw}, target)
}

func httpReturn(value Object, target any) error {
	raw, _ := json.Marshal(value)
	return copyInto(Object{"StatusCode": 200, "Body": raw}, target)
}

func TestSearchFetchLoopPreservesFieldsAndHidesTools(t *testing.T) {
	round := 0
	httpCalls := 0
	e := testEngine(t, func(method string, value any, target any) error {
		args := object(value)
		switch method {
		case "host.model.execute":
			body, _ := decodeObject(args["body"].([]byte))
			if object(body["reasoning"])["effort"] != "max" {
				t.Error("reasoning effort changed")
			}
			if object(body["metadata"])["sentinel"] != "keep" {
				t.Error("client metadata changed")
			}
			for _, raw := range array(body["tools"]) {
				if strings.HasPrefix(str(object(raw)["type"]), "web_search") {
					t.Error("native web search tool was not adapted")
				}
			}
			round++
			switch round {
			case 1:
				return modelReturn(response(toolCall(searchName, "search1", Object{"query": "official docs"})), target)
			case 2:
				if len(array(body["input"])) != 3 {
					t.Error("search tool result not continued")
				}
				return modelReturn(response(toolCall(fetchName, "fetch1", Object{"url": "https://example.com/docs"})), target)
			default:
				return modelReturn(response(textItem("verified answer")), target)
			}
		case "host.http.do":
			httpCalls++
			if strings.HasSuffix(str(args["url"]), "/search") {
				return httpReturn(Object{"results": []any{Object{"title": "docs", "url": "https://example.com/docs", "summary": "real result"}}}, target)
			}
			return httpReturn(Object{"title": "docs", "content": "# public content"}, target)
		case "host.log":
			if strings.Contains(str(args["message"]), "DUMMY_PRIVATE") {
				t.Error("secret in audit log")
			}
		default:
			t.Errorf("unexpected callback %s", method)
		}
		return nil
	})
	body := Object{"model": "test-model", "input": "use search and fetch", "reasoning": Object{"effort": "max"},
		"metadata": Object{"sentinel": "keep"}, "tools": []any{Object{"type": "web_search"}}}
	reply, err := e.Execute(execution(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(reply)
	if strings.Contains(string(raw), searchName) || strings.Contains(string(raw), fetchName) {
		t.Fatal("private gateway tool escaped to client")
	}
	_, _, text, usage := unpackReply(reply, false)
	if text != "verified answer" || httpCalls != 2 || round != 3 || usage["total_tokens"] != float64(45) {
		t.Fatalf("unexpected result text=%q calls=%d rounds=%d usage=%v", text, httpCalls, round, usage)
	}
}

func TestContinuationRoundsAndToolCallsAreNotCapped(t *testing.T) {
	const continuationRounds = 14
	modelRound := 0
	httpCalls := 0
	e := testEngine(t, func(method string, _ any, target any) error {
		switch method {
		case "host.model.execute":
			modelRound++
			if modelRound <= continuationRounds {
				id := "search-" + string(rune('a'+modelRound))
				return modelReturn(response(toolCall(searchName, id, Object{"query": "round " + id})), target)
			}
			return modelReturn(response(textItem("completed after many rounds")), target)
		case "host.http.do":
			httpCalls++
			return httpReturn(Object{"results": []any{}}, target)
		case "host.log":
			return nil
		default:
			t.Errorf("unexpected callback %s", method)
		}
		return nil
	})
	reply, err := e.Execute(execution(Object{"model": "test-model", "input": "keep searching"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, text, _ := unpackReply(reply, false)
	if text != "completed after many rounds" || modelRound != continuationRounds+1 || httpCalls != continuationRounds {
		t.Fatalf("rounds=%d http_calls=%d text=%q", modelRound, httpCalls, text)
	}
}

func TestMixedClientToolContinuationAndTenantIsolation(t *testing.T) {
	modelRound := 0
	httpCalls := 0
	e := testEngine(t, func(method string, value any, target any) error {
		args := object(value)
		switch method {
		case "host.model.execute":
			modelRound++
			body, _ := decodeObject(args["body"].([]byte))
			if modelRound == 1 {
				return modelReturn(response(
					Object{"type": "reasoning", "encrypted_content": "opaque-private-state"},
					toolCall(searchName, "owned_search", Object{"query": "docs"}),
					toolCall("client_echo", "external_echo", Object{"value": "client-must-execute"}),
				), target)
			}
			history := array(body["input"])
			hasPrivate, hasOwnedResult, hasClientResult := false, false, false
			for _, item := range history {
				v := object(item)
				hasPrivate = hasPrivate || v["encrypted_content"] == "opaque-private-state"
				hasOwnedResult = hasOwnedResult || v["call_id"] == "owned_search" && v["type"] == "function_call_output"
				hasClientResult = hasClientResult || v["output"] == "client-result"
			}
			if !hasPrivate || !hasOwnedResult || !hasClientResult {
				t.Error("lost mixed-tool or reasoning continuation state")
			}
			return modelReturn(response(textItem("continued correctly")), target)
		case "host.http.do":
			httpCalls++
			return httpReturn(Object{"results": []any{}}, target)
		}
		return nil
	})
	first := execution(Object{"input": "mixed tools", "tools": []any{
		Object{"type": "function", "name": "client_echo", "parameters": Object{"type": "object"}},
	}})
	reply, err := e.Execute(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls, _, _, _ := unpackReply(reply, false)
	if len(calls) != 1 || calls[0]["name"] != "client_echo" || httpCalls != 1 {
		t.Fatal("client-owned tool executed or filtered incorrectly")
	}
	nextBody := Object{"input": []any{
		Object{"role": "user", "content": "mixed tools"}, calls[0],
		Object{"type": "function_call_output", "call_id": "external_echo", "output": "client-result"},
	}}
	foreign := execution(nextBody)
	foreign.Headers["Authorization"] = []string{"Bearer TEST_CLIENT_B"}
	if _, err := e.Execute(foreign, nil); err == nil {
		t.Fatal("foreign client restored conversation state")
	}
	continued, err := e.Execute(execution(nextBody), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, text, _ := unpackReply(continued, false)
	if text != "continued correctly" || modelRound != 2 || httpCalls != 1 {
		t.Fatal("mixed tool continuation failed")
	}
}

func TestPassthroughAndToolNameCollision(t *testing.T) {
	e := testEngine(t, func(string, any, any) error { return nil })
	for _, body := range []Object{
		{"input": "answer", "tool_choice": "none"},
		{"input": "answer", "metadata": Object{"deepseek_search": false}},
	} {
		raw, _ := json.Marshal(body)
		if e.ShouldHandle("test-model", "openai-response", raw) {
			t.Fatal("explicit tool opt-out not honored")
		}
	}
	body := Object{"input": "answer", "tools": []any{Object{"type": "function", "name": searchName}}}
	if _, err := e.Execute(execution(body), nil); err == nil {
		t.Fatal("reserved function collision not rejected")
	}
	raw, _ := json.Marshal(Object{"input": "answer"})
	if e.ShouldHandle("unknown-model", "openai-response", raw) || e.ShouldHandle("test-model", "claude", raw) {
		t.Fatal("unsupported model/protocol should use original host route")
	}
}

func TestNoSearchRequestDoesNotCallDeepSeek(t *testing.T) {
	called := 0
	e := testEngine(t, func(method string, _ any, target any) error {
		if method != "host.model.execute" {
			called++
		}
		return modelReturn(response(textItem("ordinary answer")), target)
	})
	if _, err := e.Execute(execution(Object{"input": "answer directly"}), nil); err != nil || called != 0 {
		t.Fatal("ordinary answer made a search request", err)
	}
}

func TestFetchPolicyAndUnicodeQuery(t *testing.T) {
	for _, bad := range []string{
		"http://127.0.0.1/", "http://10.0.0.1/", "http://169.254.169.254/",
		"http://localhost/", "http://foo.internal/", "https://user:password@example.com/",
		"file:///etc/passwd", "https://[::1]/",
	} {
		if publicURL(bad) == nil {
			t.Errorf("unsafe URL accepted: %s", bad)
		}
	}
	if err := publicURL("https://example.com/article"); err != nil {
		t.Fatal(err)
	}
	called := false
	e := testEngine(t, func(method string, _ any, target any) error {
		if method == "host.http.do" {
			called = true
			return httpReturn(Object{"results": []any{}}, target)
		}
		return nil
	})
	_, err := e.executeTool(execution(Object{}), toolCall(searchName, "id", Object{"query": strings.Repeat("中", 128)}))
	if err != nil || !called {
		t.Fatal("Unicode query length checked as byte length")
	}
	called = false
	result, err := e.executeTool(execution(Object{}), toolCall(searchName, "id2", Object{"query": strings.Repeat("中", 129)}))
	if err != nil || result["ok"] != false || called {
		t.Fatal("oversized query was sent upstream")
	}
}

func TestFetchedContentIsNotTruncated(t *testing.T) {
	content := strings.Repeat("x", 20000)
	e := testEngine(t, func(method string, _ any, target any) error {
		if method == "host.http.do" {
			return httpReturn(Object{"title": "long page", "content": content}, target)
		}
		return nil
	})
	result, err := e.executeTool(execution(Object{}), toolCall(fetchName, "fetch", Object{
		"url": "https://example.com/long-page"}))
	if err != nil || result["content"] != content {
		t.Fatal("fetched content was truncated", err, len(str(result["content"])))
	}
}

func TestRepeatedToolCallAndStatePersistence(t *testing.T) {
	httpCalls := 0
	e := testEngine(t, func(method string, _ any, target any) error {
		if method == "host.http.do" {
			httpCalls++
			return httpReturn(Object{"results": []any{}}, target)
		}
		return nil
	})
	call := toolCall(searchName, "id", Object{"query": "fixed query"})
	for index := 0; index < 3; index++ {
		if _, err := e.executeTool(execution(Object{}), call); err != nil {
			t.Fatal(err)
		}
	}
	if httpCalls != 3 {
		t.Fatal("repeated tool calls were suppressed", httpCalls)
	}
	req := execution(Object{})
	e.remember(req, "previous-id", nil, []any{Object{"role": "user", "content": "history"}})
	history, err := e.restore(req, Object{"previous_response_id": "previous-id", "input": []any{
		Object{"role": "user", "content": "new message"}}}, false)
	if err != nil || len(history) != 2 {
		t.Fatal("conversation state was not retained", err, len(history))
	}
}

func TestStreamIsIncrementalAndOwnedCallsHidden(t *testing.T) {
	modelRound := 0
	streams := map[string][]Object{}
	var chunks [][]byte
	firstTextSent := false
	e := testEngine(t, func(method string, value any, target any) error {
		args := object(value)
		switch method {
		case "host.model.execute_stream":
			modelRound++
			id := string(rune('a' + modelRound))
			if modelRound == 1 {
				streams[id] = []Object{{"type": "response.completed", "response": response(
					toolCall(searchName, "s", Object{"query": "docs"}))}}
			} else {
				streams[id] = []Object{
					{"type": "response.output_text.delta", "delta": "first "},
					{"type": "response.output_text.delta", "delta": "second"},
					{"type": "response.completed", "response": response(textItem("first second"))},
				}
			}
			return copyInto(Object{"status_code": 200, "stream_id": id}, target)
		case "host.model.stream_read":
			id := str(args["stream_id"])
			events := streams[id]
			if len(events) == 0 {
				return copyInto(Object{"done": true}, target)
			}
			if modelRound == 2 && len(events) == 1 && !firstTextSent {
				t.Fatal("text was buffered until upstream completion")
			}
			raw, _ := json.Marshal(events[0])
			streams[id] = events[1:]
			frame := append(append([]byte("data: "), raw...), []byte("\n\n")...)
			return copyInto(Object{"payload": frame, "done": false}, target)
		case "host.model.stream_close":
			return nil
		case "host.http.do":
			return httpReturn(Object{"results": []any{}}, target)
		case "host.log":
			return nil
		}
		return nil
	})
	render := NewRenderer("test-model", false, func(raw []byte) error {
		chunks = append(chunks, raw)
		if strings.Contains(string(raw), `"delta":"first "`) {
			firstTextSent = true
		}
		return nil
	})
	if err := render.Begin(); err != nil {
		t.Fatal(err)
	}
	req := execution(Object{"input": "search and answer", "stream": true})
	reply, err := e.Execute(req, render)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, chunk := range chunks {
		joined += string(chunk)
	}
	if !firstTextSent || strings.Contains(joined, searchName) || strings.Count(joined, "event: response.completed\n") != 1 {
		t.Fatal("incorrect streaming lifecycle or leaked gateway call")
	}
	if reply["id"] != render.id || reply["status"] != "completed" {
		t.Fatal("inconsistent response ID/status")
	}
}

func TestSSEParserSplitFrames(t *testing.T) {
	p := sseParser{}
	count := 0
	handle := func(value Object) error {
		count++
		if value["type"] != "test" {
			t.Error("unexpected event")
		}
		return nil
	}
	for _, data := range []string{"event: te", "st\r\ndata: {\"type\":\"test\"}\r\n", "\r\ndata: [DONE]\n\n"} {
		if err := p.Feed([]byte(data), handle); err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatal("split SSE frame lost or duplicated")
	}
}

func TestConcurrentToolAndStateAccess(t *testing.T) {
	e := testEngine(t, func(method string, _ any, target any) error {
		if method == "host.http.do" {
			return httpReturn(Object{"results": []any{}}, target)
		}
		return nil
	})
	var wg sync.WaitGroup
	for index := 0; index < 12; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.executeTool(execution(Object{}), toolCall(searchName, "id", Object{"query": "shared public query"}))
			e.remember(execution(Object{}), randomID("response"), nil, []any{})
		}()
	}
	wg.Wait()
}

func TestParserAcceptsNativeHostJSONEvents(t *testing.T) {
	parser := sseParser{}
	count := 0
	handle := func(value Object) error {
		count++
		if value["type"] != "response.completed" {
			t.Fatal("wrong native event")
		}
		return nil
	}
	for _, raw := range []string{
		`{"type":"response.completed","response":{"status":"completed"}}`,
		"data: {\"type\":\"response.completed\",\"response\":{}}\n",
		`{"type":"response.comp`,
		`leted","response":{}}`,
		"[DONE]",
	} {
		if err := parser.Feed([]byte(raw), handle); err != nil {
			t.Fatal(err)
		}
	}
	if count != 3 {
		t.Fatal("native JSON events lost or duplicated", count)
	}
}

func TestDeepSeekOwnedChoiceFallbackPreservesReasoning(t *testing.T) {
	attempts := 0
	e := testEngine(t, func(method string, value any, target any) error {
		if method != "host.model.execute" {
			return nil
		}
		attempts++
		body, _ := decodeObject(object(value)["body"].([]byte))
		if attempts == 1 {
			return &RPCError{Message: "Thinking mode does not support this tool_choice", HTTPStatus: 400}
		}
		if body["tool_choice"] != "auto" || object(body["reasoning"])["effort"] != "max" {
			t.Fatal("fallback modified thinking or failed to adapt owned choice")
		}
		if !strings.Contains(str(body["instructions"]), "gateway_web_search") {
			t.Fatal("fallback lost the explicit selected tool requirement")
		}
		return modelReturn(response(textItem("OK")), target)
	})
	body := Object{"input": "search", "reasoning": Object{"effort": "max"},
		"tools": []any{Object{"type": "web_search"}}, "tool_choice": Object{"type": "web_search"}}
	if _, err := e.Execute(execution(body), nil); err != nil || attempts != 2 {
		t.Fatal("owned tool choice adaptation failed", err, attempts)
	}
}

func TestJovisNamedChoiceFallbackIsNarrow(t *testing.T) {
	attempts := 0
	e := testEngine(t, func(method string, value any, target any) error {
		if method != "host.model.execute" {
			return nil
		}
		attempts++
		body, _ := decodeObject(object(value)["body"].([]byte))
		if attempts == 1 {
			return &RPCError{Message: `tool_choice: type "tool" and "any" are not supported for this model`, HTTPStatus: 400}
		}
		if body["tool_choice"] != "auto" {
			t.Fatal("Jovis named choice was not relaxed")
		}
		return modelReturn(response(textItem("OK")), target)
	})
	body := Object{"input": "search", "tools": []any{Object{"type": "web_search"}},
		"tool_choice": Object{"type": "web_search"}}
	if _, err := e.Execute(execution(body), nil); err != nil || attempts != 2 {
		t.Fatal("Jovis owned choice adaptation failed", err, attempts)
	}
	clientBody := Object{"tool_choice": Object{"type": "function", "name": "client_echo"}}
	adaptOwnedChoice(clientBody, nil, false)
	if object(clientBody["tool_choice"])["name"] != "client_echo" {
		t.Fatal("client-owned named choice was changed")
	}
}

func TestChatRendererLeavesSSEFramingToHost(t *testing.T) {
	chunks := [][]byte{}
	render := NewRenderer("test-model", true, func(payload []byte) error {
		chunks = append(chunks, payload)
		if !json.Valid(payload) {
			t.Fatal("Chat executor emitted SSE framing instead of JSON")
		}
		return nil
	})
	if err := render.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := render.Text("text"); err != nil {
		t.Fatal(err)
	}
	result := finalReply(render.id, "test-model", "text", nil, Object{}, true)
	if err := render.Finish(result, nil); err != nil || len(chunks) != 3 {
		t.Fatal("Chat framing lifecycle incorrect", err, len(chunks))
	}
}

func TestIdenticalReconfigureKeepsActiveEngine(t *testing.T) {
	folder := t.TempDir()
	key := filepath.Join(folder, "key")
	settings := filepath.Join(folder, "settings.json")
	_ = os.WriteFile(key, []byte("DUMMY_PRIVATE_TEST_KEY"), 0600)
	cfg, _ := json.Marshal(Config{KeyFile: key})
	_ = os.WriteFile(settings, cfg, 0600)
	request, _ := json.Marshal(map[string]any{"schema_version": 3,
		"config_yaml": []byte("settings_file: " + settings + "\n")})
	pluginState.Lock()
	old := pluginState.engine
	pluginState.engine = nil
	pluginState.Unlock()
	defer func() {
		pluginState.Lock()
		pluginState.engine.Close()
		pluginState.engine = old
		pluginState.Unlock()
	}()
	if _, err := dispatch("plugin.register", request); err != nil {
		t.Fatal(err)
	}
	first := pluginState.engine
	first.remember(execution(Object{}), "retained-state", nil, []any{})
	if _, err := dispatch("plugin.reconfigure", request); err != nil {
		t.Fatal(err)
	}
	if pluginState.engine != first || first.closed || len(first.states) != 1 {
		t.Fatal("unchanged reconfigure invalidated active engine/state")
	}
}

func TestConfiguredAutoChoiceIsAppliedBeforeFirstModelCall(t *testing.T) {
	attempts := 0
	e := testEngine(t, func(method string, value any, target any) error {
		if method != "host.model.execute" {
			return nil
		}
		attempts++
		body, _ := decodeObject(object(value)["body"].([]byte))
		if body["tool_choice"] != "auto" || object(body["reasoning"])["effort"] != "high" {
			t.Fatal("configured compatibility did not preserve reasoning or adapt choice before sending")
		}
		return modelReturn(response(textItem("OK")), target)
	})
	e.cfg.AutoChoiceModels = []string{"test-model"}
	request := execution(Object{"input": "search", "reasoning": Object{"effort": "high"},
		"tools": []any{Object{"type": "web_search"}}, "tool_choice": Object{"type": "web_search"}})
	if _, err := e.Execute(request, nil); err != nil || attempts != 1 {
		t.Fatal("upfront compatibility failed", err, attempts)
	}
	body := Object{"tool_choice": Object{"type": "function", "name": "client_echo"}}
	adaptOwnedChoice(body, nil, false)
	if object(body["tool_choice"])["name"] != "client_echo" {
		t.Fatal("client-owned tool selection changed")
	}
}

func TestParserAcceptsTrimmedVendorSSEFrames(t *testing.T) {
	parser := sseParser{}
	count := 0
	handle := func(value Object) error {
		count++
		if value["type"] != "response.completed" {
			t.Fatal("vendor SSE event type was lost")
		}
		return nil
	}
	for _, raw := range []string{
		"event: response.completed\ndata: {\"response\":{\"status\":\"completed\"}}",
		"event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{}}\r\n\r\n",
		"event: response.completed\ndata: {\"respon",
		"se\":{\"status\":\"completed\"}}",
	} {
		if err := parser.Feed([]byte(raw), handle); err != nil {
			t.Fatal(err)
		}
	}
	if count != 3 {
		t.Fatal("trimmed/fragmented vendor SSE frames were lost", count)
	}
}

func TestDeepSeekProExcludedAcrossAliases(t *testing.T) {
	e := testEngine(t, func(string, any, any) error { return nil })
	e.cfg.Models = nil
	e.cfg.ExcludedModels = []string{"opaque-campus-alias"}
	raw, _ := json.Marshal(Object{"input": "search"})
	for _, name := range []string{"deepseek-pro", "deepseek-v4-pro-hkust",
		"DeepSeek_Pro_Qoder", "deepseekpro", "opaque-campus-alias"} {
		if e.ShouldHandle(name, "openai-response", raw) {
			t.Fatal("excluded Pro alias was routed to plugin", name)
		}
	}
	if !e.ShouldHandle("deepseek-v4-flash-hkust", "openai-response", raw) {
		t.Fatal("Flash was accidentally excluded")
	}
}

func TestCustomClientToolsAndNativeOutputItemsArePreserved(t *testing.T) {
	round := 0
	reasoning := Object{"id": "reasoning_item", "type": "reasoning",
		"encrypted_content": "opaque-private-state", "summary": []any{}}
	custom := Object{"id": "custom_item", "type": "custom_tool_call", "call_id": "custom_call",
		"name": "apply_patch", "input": "*** Begin Patch\n*** End Patch", "namespace": "client"}
	hosted := Object{"id": "hosted_item", "type": "code_interpreter_call", "status": "completed",
		"outputs": []any{Object{"type": "logs", "logs": "native output"}}}
	e := testEngine(t, func(method string, value any, target any) error {
		if method != "host.model.execute" {
			t.Fatal("custom client tool was executed on the server", method)
		}
		round++
		if round == 1 {
			return modelReturn(response(reasoning, custom, hosted), target)
		}
		body, _ := decodeObject(object(value)["body"].([]byte))
		found := false
		for _, raw := range array(body["input"]) {
			item := object(raw)
			if item["type"] == "custom_tool_call_output" && item["call_id"] == "custom_call" {
				found = true
			}
		}
		if !found {
			t.Fatal("custom tool result was not restored")
		}
		return modelReturn(response(textItem("custom result accepted")), target)
	})
	first, err := e.Execute(execution(Object{"input": "use custom tool"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	output := array(first["output"])
	if len(output) != 3 || object(output[0])["type"] != "reasoning" ||
		object(output[1])["input"] != custom["input"] ||
		object(output[1])["namespace"] != "client" || object(output[2])["id"] != "hosted_item" {
		t.Fatal("custom/native item fields were lost", output)
	}
	payload := Object{"input": append(output, Object{"type": "custom_tool_call_output",
		"call_id": "custom_call", "output": "client applied patch"})}
	if _, err := e.Execute(execution(payload), nil); err != nil {
		t.Fatal(err)
	}
	var frames []byte
	render := NewRenderer("test-model", false, func(data []byte) error {
		frames = append(frames, data...)
		return nil
	})
	if err := render.Begin(); err != nil {
		t.Fatal(err)
	}
	reply := finalReply(render.id, "test-model", "", []Object{custom}, Object{}, false)
	if err := render.Finish(reply, []Object{custom}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(frames), "response.custom_tool_call_input.delta") ||
		strings.Contains(string(frames), "response.function_call_arguments.delta") {
		t.Fatal("wrong stream event type for a custom client tool")
	}
}

func TestNaiveTextToolCallIsNarrowlyAdapted(t *testing.T) {
	round := 0
	e := testEngine(t, func(method string, value any, target any) error {
		if method == "host.http.do" {
			return httpReturn(Object{"results": []any{Object{"title": "docs",
				"url": "https://api-docs.deepseek.com/"}}}, target)
		}
		if method == "host.log" {
			return nil
		}
		if method != "host.model.execute" {
			t.Fatal("text tool call reached another subsystem", method)
		}
		round++
		body, _ := decodeObject(object(value)["body"].([]byte))
		if round == 1 {
			text := "\n<tool_call>\n<function=gateway_web_search>\n" +
				"<parameter=query>site:api-docs.deepseek.com search fetch</parameter>\n" +
				"</function>\n</tool_call>"
			return modelReturn(response(Object{"type": "reasoning", "id": "rs", "summary": []any{}},
				textItem(text)), target)
		}
		history := array(body["input"])
		foundCall, foundOutput := false, false
		for _, raw := range history {
			item := object(raw)
			foundCall = foundCall || item["type"] == "function_call" &&
				item["name"] == searchName && item["call_id"] != ""
			foundOutput = foundOutput || item["type"] == "function_call_output" &&
				str(item["call_id"]) != ""
		}
		if !foundCall || !foundOutput {
			t.Fatal("adapted text tool call was not a complete function round trip")
		}
		return modelReturn(response(textItem("text-tool answer")), target)
	})
	e.cfg.TextToolCallModels = []string{"test-model"}
	reply, err := e.Execute(execution(Object{"input": "search"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, text, _ := unpackReply(reply, false)
	if text != "text-tool answer" || round != 2 || strings.Contains(jsonString(reply), "<tool_call>") {
		t.Fatal("Naive text tool adapter failed", text, round, jsonString(reply))
	}
	call, clean, ok := parseTextToolCall("test-model",
		"prefix <tool_call><function=gateway_web_fetch><parameter=url>https://example.com</parameter></function></tool_call> suffix",
		[]string{"test-model"})
	if !ok || call["name"] != fetchName || clean != "prefix  suffix" {
		t.Fatal("fetch text tool call was not parsed")
	}
	if _, _, ok := parseTextToolCall("other-model", "<tool_call><function=gateway_web_search>"+
		"<parameter=query>x</parameter></function></tool_call>", []string{"test-model"}); ok {
		t.Fatal("text tool adapter was enabled for an unauthorized model")
	}
}

func TestNaiveTextToolCallWorksOnChatProtocol(t *testing.T) {
	round := 0
	e := testEngine(t, func(method string, value any, target any) error {
		switch method {
		case "host.http.do":
			return httpReturn(Object{"results": []any{}}, target)
		case "host.log":
			return nil
		case "host.model.execute":
			round++
			body, _ := decodeObject(object(value)["body"].([]byte))
			if round == 1 {
				text := "<tool_call><function=gateway_web_search>" +
					"<parameter=query>docs</parameter><parameter=count>3</parameter>" +
					"</function></tool_call>"
				return copyInto(Object{"status_code": 200, "body": []byte(jsonString(Object{
					"choices": []any{Object{"message": Object{"role": "assistant", "content": text},
						"finish_reason": "stop"}}, "usage": Object{"prompt_tokens": 1,
						"completion_tokens": 1, "total_tokens": 2}}))}, target)
			}
			messages := array(body["messages"])
			validAssistant, validTool := false, false
			for _, raw := range messages {
				message := object(raw)
				validAssistant = validAssistant || message["role"] == "assistant" &&
					len(array(message["tool_calls"])) == 1 && message["content"] == nil
				validTool = validTool || message["role"] == "tool" && message["tool_call_id"] != ""
			}
			if !validAssistant || !validTool {
				t.Fatal("Chat text tool call did not become a valid tool round trip")
			}
			return copyInto(Object{"status_code": 200, "body": []byte(jsonString(Object{
				"choices": []any{Object{"message": Object{"role": "assistant",
					"content": "chat text-tool answer"}, "finish_reason": "stop"}},
				"usage": Object{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}))}, target)
		}
		t.Fatal("unexpected callback", method)
		return nil
	})
	e.cfg.TextToolCallModels = []string{"test-model"}
	reply, err := e.Execute(execution(Object{"model": "test-model", "messages": []any{
		Object{"role": "user", "content": "search"}}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer := str(object(object(array(reply["choices"])[0])["message"])["content"]); answer != "chat text-tool answer" {
		t.Fatal("wrong Chat answer", answer)
	}
}

func TestConversationStateHasNoEvictionOrSizeCap(t *testing.T) {
	e := testEngine(t, func(string, any, any) error { return nil })
	request := execution(Object{})
	for _, id := range []string{"oldest", "middle", "newest"} {
		e.remember(request, id, nil, []any{Object{"role": "user", "content": id}})
	}
	large := strings.Repeat("x", 2<<20)
	e.remember(request, "oversized", nil, []any{Object{"role": "user", "content": large}})
	if len(e.states) != 4 {
		t.Fatal("conversation state was evicted", len(e.states))
	}
	history, err := e.restore(request, Object{"previous_response_id": "oversized", "input": []any{
		Object{"role": "user", "content": "next"}}}, false)
	if err != nil || len(history) != 2 {
		t.Fatal("large conversation state was discarded", err, len(history))
	}
}

func jsonString(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
