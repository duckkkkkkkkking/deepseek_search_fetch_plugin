package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type Renderer struct {
	id      string
	model   string
	chat    bool
	text    string
	started bool
	seq     int
	send    func([]byte) error
}

func NewRenderer(model string, chat bool, send func([]byte) error) *Renderer {
	prefix := "resp_ds_"
	if chat {
		prefix = "chatcmpl-ds-"
	}
	return &Renderer{id: randomID(prefix), model: model, chat: chat, send: send}
}

func (r *Renderer) event(kind string, payload Object) error {
	payload["type"] = kind
	payload["sequence_number"] = r.seq
	r.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return r.send(append(append([]byte("event: "+kind+"\ndata: "), raw...), []byte("\n\n")...))
}

func (r *Renderer) chatChunk(delta Object, finish any, usage any) error {
	payload := Object{"id": r.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": r.model, "choices": []any{Object{"index": 0, "delta": delta, "finish_reason": finish}}}
	if usage != nil {
		payload["usage"] = usage
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// The public Chat Completions handler adds its own SSE data framing.
	return r.send(raw)
}

func (r *Renderer) Begin() error {
	if r.chat {
		return r.chatChunk(Object{"role": "assistant", "content": ""}, nil, nil)
	}
	reply := finalReply(r.id, r.model, "", nil, nil, false)
	reply["status"] = "in_progress"
	if err := r.event("response.created", Object{"response": reply}); err != nil {
		return err
	}
	return r.event("response.in_progress", Object{"response": reply})
}

func (r *Renderer) Text(text string) error {
	if text == "" {
		return nil
	}
	r.text += text
	if r.chat {
		return r.chatChunk(Object{"content": text}, nil, nil)
	}
	if !r.started {
		r.started = true
		item := Object{"id": r.id + "_msg", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
		if err := r.event("response.output_item.added", Object{"output_index": 0, "item": item}); err != nil {
			return err
		}
		if err := r.event("response.content_part.added", Object{"item_id": r.id + "_msg", "output_index": 0,
			"content_index": 0, "part": Object{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
	}
	return r.event("response.output_text.delta", Object{"item_id": r.id + "_msg", "output_index": 0, "content_index": 0, "delta": text})
}

func (r *Renderer) Finish(reply Object, external []Object) error {
	if r.chat {
		calls := []any{}
		for index, call := range external {
			calls = append(calls, Object{"index": index, "id": call["call_id"], "type": "function",
				"function": Object{"name": call["name"], "arguments": call["arguments"]}})
		}
		if len(calls) > 0 {
			if err := r.chatChunk(Object{"tool_calls": calls}, nil, nil); err != nil {
				return err
			}
		}
		choice := object(array(reply["choices"])[0])
		if err := r.chatChunk(Object{}, choice["finish_reason"], reply["usage"]); err != nil {
			return err
		}
		// The public Chat Completions handler emits [DONE] when this stream closes.
		return nil
	}
	index := 0
	if r.started {
		part := Object{"type": "output_text", "text": r.text, "annotations": []any{}}
		if err := r.event("response.output_text.done", Object{"item_id": r.id + "_msg", "output_index": 0,
			"content_index": 0, "text": r.text}); err != nil {
			return err
		}
		if err := r.event("response.content_part.done", Object{"item_id": r.id + "_msg", "output_index": 0,
			"content_index": 0, "part": part}); err != nil {
			return err
		}
		item := object(array(reply["output"])[0])
		if err := r.event("response.output_item.done", Object{"output_index": 0, "item": item}); err != nil {
			return err
		}
		index = 1
	}
	for _, call := range external {
		item := object(clone(call))
		if str(item["id"]) == "" {
			item["id"] = randomID("fc_")
			call["id"] = item["id"]
		}
		kind := str(call["type"])
		if kind == "function_call" {
			item["arguments"], item["status"] = "", "in_progress"
		} else if kind == "custom_tool_call" {
			item["input"] = ""
		}
		if err := r.event("response.output_item.added", Object{"output_index": index, "item": item}); err != nil {
			return err
		}
		if kind == "function_call" {
			if err := r.event("response.function_call_arguments.delta", Object{"item_id": item["id"],
				"output_index": index, "delta": call["arguments"]}); err != nil {
				return err
			}
			if err := r.event("response.function_call_arguments.done", Object{"item_id": item["id"],
				"output_index": index, "arguments": call["arguments"]}); err != nil {
				return err
			}
		} else if kind == "custom_tool_call" {
			if err := r.event("response.custom_tool_call_input.delta", Object{"item_id": item["id"],
				"output_index": index, "delta": call["input"]}); err != nil {
				return err
			}
			if err := r.event("response.custom_tool_call_input.done", Object{"item_id": item["id"],
				"output_index": index, "input": call["input"]}); err != nil {
				return err
			}
		}
		if err := r.event("response.output_item.done", Object{"output_index": index, "item": call}); err != nil {
			return err
		}
		index++
	}
	for _, raw := range array(reply["output"]) {
		item := object(raw)
		kind := str(item["type"])
		if kind == "message" || kind == "reasoning" || kind == "function_call" ||
			kind == "custom_tool_call" || str(item["call_id"]) != "" {
			continue
		}
		if err := r.event("response.output_item.added", Object{"output_index": index, "item": item}); err != nil {
			return err
		}
		if err := r.event("response.output_item.done", Object{"output_index": index, "item": item}); err != nil {
			return err
		}
		index++
	}
	kind := "response.completed"
	if reply["status"] == "incomplete" {
		kind = "response.incomplete"
	}
	return r.event(kind, Object{"response": reply})
}

func (e *Engine) ExecuteStream(request Execution) {
	body, err := decodeObject(request.Payload)
	chat := body != nil && body["messages"] != nil
	send := func(data []byte) error {
		return e.call("host.stream.emit", Object{"stream_id": request.StreamID, "payload": data}, nil)
	}
	render := NewRenderer(request.Model, chat, send)
	if err == nil {
		err = render.Begin()
	}
	if err == nil {
		_, err = e.Execute(request, render)
	}
	if err != nil {
		message := safeError(err)
		if chat {
			raw, _ := json.Marshal(Object{"error": Object{"type": "deepseek_search_error", "message": message}})
			_ = send(raw)
		} else {
			_ = render.event("response.failed", Object{"response": Object{
				"id": render.id, "object": "response", "status": "failed",
				"error": Object{"code": "deepseek_search_error", "message": message},
			}})
		}
	}
	_ = e.call("host.stream.close", Object{"stream_id": request.StreamID}, nil)
}

type sseParser struct {
	pending []byte
}

func (p *sseParser) Feed(raw []byte, handle func(Object) error) error {
	if len(p.pending) == 0 {
		if value, ok := standaloneSSE(raw); ok {
			if value == nil {
				return nil
			}
			return handle(value)
		}
		payload := bytes.TrimSpace(raw)
		if bytes.Equal(payload, []byte("[DONE]")) || bytes.Equal(payload, []byte("data: [DONE]")) {
			return nil
		}
		if bytes.HasPrefix(payload, []byte("data:")) {
			payload = bytes.TrimSpace(bytes.TrimPrefix(payload, []byte("data:")))
		}
		// Native Codex host callbacks expose complete JSON events without the
		// SSE wrapping added later by the public HTTP handler.
		if bytes.HasPrefix(payload, []byte("{")) && json.Valid(payload) {
			value, err := decodeObject(payload)
			if err != nil {
				return err
			}
			return handle(value)
		}
	}
	p.pending = append(p.pending, raw...)
	if len(p.pending) > 8*1024*1024 {
		return fmt.Errorf("upstream SSE frame exceeds plugin size limit")
	}
	p.pending = bytes.ReplaceAll(p.pending, []byte("\r\n"), []byte("\n"))
	if value, ok := standaloneSSE(p.pending); ok {
		p.pending = nil
		if value == nil {
			return nil
		}
		return handle(value)
	}
	if payload := bytes.TrimSpace(p.pending); bytes.HasPrefix(payload, []byte("{")) && json.Valid(payload) {
		p.pending = nil
		value, err := decodeObject(payload)
		if err != nil {
			return err
		}
		return handle(value)
	}
	for {
		index := bytes.Index(p.pending, []byte("\n\n"))
		if index < 0 {
			break
		}
		frame := string(p.pending[:index])
		p.pending = p.pending[index+2:]
		var data []string
		eventName := ""
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		value := strings.Join(data, "\n")
		if value == "" || value == "[DONE]" {
			continue
		}
		obj, err := decodeObject([]byte(value))
		if err != nil {
			return fmt.Errorf("upstream sent invalid SSE JSON")
		}
		if obj["type"] == nil && eventName != "" {
			obj["type"] = eventName
		}
		if err := handle(obj); err != nil {
			return err
		}
	}
	return nil
}

func standaloneSSE(raw []byte) (Object, bool) {
	text := strings.ReplaceAll(strings.TrimSpace(string(raw)), "\r\n", "\n")
	if !strings.HasPrefix(text, "event:") {
		return nil, false
	}
	name := ""
	data := []string{}
	eventCount := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "event:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			eventCount++
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if eventCount != 1 || len(data) == 0 {
		return nil, false
	}
	joined := strings.Join(data, "\n")
	if joined == "[DONE]" {
		return nil, true
	}
	value, err := decodeObject([]byte(joined))
	if err != nil {
		return nil, false
	}
	if value["type"] == nil {
		value["type"] = name
	}
	return value, true
}

func (e *Engine) modelStream(request Execution, body Object, chat bool, emit func(string) error) (Object, error) {
	var started struct {
		Status int    `json:"status_code"`
		ID     string `json:"stream_id"`
	}
	if err := e.call("host.model.execute_stream", e.modelRequest(request, body, chat, true), &started); err != nil {
		return nil, err
	}
	if started.ID == "" {
		return nil, fmt.Errorf("host model callback returned no stream")
	}
	defer func() {
		_ = e.call("host.model.stream_close", Object{"stream_id": started.ID}, nil)
	}()
	parser := sseParser{}
	final := Object{}
	message := Object{"role": "assistant", "content": ""}
	functions := map[int]Object{}
	text := ""
	finish := "stop"
	usage := Object{}
	packets := 0
	events := 0
	shapes := []string{}
	handle := func(value Object) error {
		events++
		if value["error"] != nil || value["type"] == "error" || value["type"] == "response.failed" {
			message := str(object(value["error"])["message"])
			if unsupportedNamedToolChoice(message) {
				return &RPCError{Message: "Upstream does not support this named tool_choice", HTTPStatus: 400}
			}
			return fmt.Errorf("upstream model stream failed")
		}
		if chat {
			if value["usage"] != nil {
				usage = object(value["usage"])
			}
			for _, entry := range array(value["choices"]) {
				choice := object(entry)
				delta := object(choice["delta"])
				part := str(delta["content"])
				text += part
				if part != "" {
					if err := emit(part); err != nil {
						return err
					}
				}
				if reasoning := str(delta["reasoning_content"]); reasoning != "" {
					message["reasoning_content"] = str(message["reasoning_content"]) + reasoning
				}
				for _, raw := range array(delta["tool_calls"]) {
					tool := object(raw)
					index, _ := tool["index"].(float64)
					item := functions[int(index)]
					if item == nil {
						item = Object{"id": "", "type": "function", "function": Object{"name": "", "arguments": ""}}
						functions[int(index)] = item
					}
					if str(tool["id"]) != "" {
						item["id"] = tool["id"]
					}
					fn := object(item["function"])
					deltaFn := object(tool["function"])
					if str(deltaFn["name"]) != "" {
						fn["name"] = str(fn["name"]) + str(deltaFn["name"])
					}
					fn["arguments"] = str(fn["arguments"]) + str(deltaFn["arguments"])
				}
				if str(choice["finish_reason"]) != "" {
					finish = str(choice["finish_reason"])
				}
			}
			return nil
		}
		if value["type"] == "response.output_text.delta" {
			part := str(value["delta"])
			text += part
			return emit(part)
		}
		if value["type"] == "response.completed" || value["type"] == "response.incomplete" {
			final = object(value["response"])
			if len(final) == 0 && value["object"] == "response" {
				final = value
			}
			// Some host translators omit status on the terminal response object.
			// The enclosing SSE terminal event is authoritative.
			final["status"] = "completed"
			if value["type"] == "response.incomplete" {
				final["status"] = "incomplete"
			}
		}
		return nil
	}
	for {
		var chunk struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		if err := e.call("host.model.stream_read", Object{"stream_id": started.ID}, &chunk); err != nil {
			return nil, err
		}
		if chunk.Error != "" {
			if unsupportedNamedToolChoice(chunk.Error) {
				return nil, &RPCError{Message: "Upstream does not support this named tool_choice", HTTPStatus: 400}
			}
			return nil, fmt.Errorf("upstream model stream read failed")
		}
		packets++
		if len(shapes) < 4 {
			shapes = append(shapes, streamShape(chunk.Payload))
		}
		if err := parser.Feed(chunk.Payload, handle); err != nil {
			return nil, err
		}
		if chunk.Done {
			break
		}
	}
	if chat {
		message["content"] = text
		tools := []any{}
		for index := 0; index < len(functions); index++ {
			if item := functions[index]; item != nil {
				tools = append(tools, item)
			}
		}
		if len(tools) > 0 {
			message["tool_calls"] = tools
		}
		return Object{"choices": []any{Object{"message": message, "finish_reason": finish}}, "usage": usage}, nil
	}
	if len(final) == 0 {
		return nil, fmt.Errorf("model stream ended without a terminal response (status=%d packets=%d events=%d shapes=%s)",
			started.Status, packets, events, strings.Join(shapes, ";"))
	}
	_, _, wholeText, _ := unpackReply(final, false)
	if strings.HasPrefix(wholeText, text) && len(wholeText) > len(text) {
		if err := emit(wholeText[len(text):]); err != nil {
			return nil, err
		}
	}
	return final, nil
}

func streamShape(raw []byte) string {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 {
		return "empty"
	}
	if bytes.HasPrefix(value, []byte("data:")) {
		value = bytes.TrimSpace(bytes.TrimPrefix(value, []byte("data:")))
	}
	var object map[string]any
	if json.Unmarshal(value, &object) == nil && object != nil {
		keys := []string{}
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return "json-keys:" + strings.Join(keys, ",")
	}
	if bytes.HasPrefix(value, []byte("event:")) {
		return "sse"
	}
	return fmt.Sprintf("other-bytes:%d", len(raw))
}
