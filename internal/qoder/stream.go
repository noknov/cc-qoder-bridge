package qoder

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type Usage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
}
type Response struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []map[string]any `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        Usage            `json:"usage"`
}
type Emit func(string, any) error

type toolState struct {
	ID, Name string
	Args     strings.Builder
}

// Translate consumes bounded SSE frames. It emits success only after an upstream finish reason.
// Tool arguments are assembled and validated before exposing an executable tool_use block.
func Translate(reader io.Reader, model string, emit Emit) (Response, error) {
	out := Response{ID: "msg_" + randomID(), Type: "message", Role: "assistant", Model: model, Content: []map[string]any{}}
	if emit != nil {
		if err := emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": out.ID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": out.Usage}}); err != nil {
			return out, err
		}
	}
	tools := map[int]*toolState{}
	var text strings.Builder
	textOpen, finished := false, false
	retained := 0
	finishReason := ""
	closeText := func() error {
		if !textOpen {
			return nil
		}
		textOpen = false
		if emit != nil {
			return emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		}
		return nil
	}
	consume := func(data string) error {
		if data == "" || data == "[DONE]" {
			return nil
		}
		var envelope struct {
			Status int             `json:"statusCodeValue"`
			Body   json.RawMessage `json:"body"`
		}
		if json.Unmarshal([]byte(data), &envelope) != nil {
			return errors.New("invalid Qoder SSE envelope")
		}
		if envelope.Status != 0 && envelope.Status != 200 {
			if delay, queued := queueDelay(envelope.Body, 0); queued {
				return &UpstreamError{Status: 429, RetryAfter: strconv.Itoa(delay)}
			}
			return &UpstreamError{Status: envelope.Status}
		}
		if len(envelope.Body) == 0 || string(envelope.Body) == "null" {
			return nil
		}
		raw := envelope.Body
		var body string
		if json.Unmarshal(raw, &body) == nil {
			if body == "[DONE]" {
				return nil
			}
			raw = []byte(body)
		}
		var event struct {
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
				Details    struct {
					Cached  int `json:"cached_tokens"`
					Written int `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
					Calls   []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return errors.New("invalid Qoder stream payload")
		}
		if u := event.Usage; u != nil {
			out.Usage = Usage{max(0, u.Prompt-u.Details.Cached-u.Details.Written), u.Completion, u.Details.Cached, u.Details.Written}
		}
		for _, choice := range event.Choices {
			if finished {
				return errors.New("unexpected content after Qoder finish reason")
			}
			if chunk := choice.Delta.Content; chunk != "" {
				retained += len(chunk)
				if retained > 16<<20 {
					return errors.New("Qoder response exceeds 16 MiB")
				}
				if !textOpen {
					textOpen = true
					if emit != nil {
						if err := emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
							return err
						}
					}
				}
				text.WriteString(chunk)
				if emit != nil {
					if err := emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": chunk}}); err != nil {
						return err
					}
				}
			}
			for _, call := range choice.Delta.Calls {
				if call.Index < 0 || call.Index >= 128 {
					return errors.New("invalid Qoder tool index")
				}
				t := tools[call.Index]
				if t == nil {
					t = &toolState{}
					tools[call.Index] = t
				}
				if call.ID != "" {
					if t.ID != "" && t.ID != call.ID {
						return errors.New("conflicting tool IDs")
					}
					t.ID = call.ID
				}
				if call.Function.Name != "" {
					if t.Name != "" && t.Name != call.Function.Name {
						return errors.New("conflicting tool names")
					}
					t.Name = call.Function.Name
				}
				retained += len(call.Function.Arguments)
				if retained > 16<<20 {
					return errors.New("Qoder response exceeds 16 MiB")
				}
				t.Args.WriteString(call.Function.Arguments)
			}
			if choice.Finish != "" {
				finishReason = choice.Finish
				finished = true
			}
		}
		return nil
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var frame strings.Builder
	flush := func() error { data := strings.TrimSuffix(frame.String(), "\n"); frame.Reset(); return consume(data) }
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return out, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if frame.Len()+len(value) > 4<<20 {
				return out, errors.New("Qoder SSE frame exceeds 4 MiB")
			}
			frame.WriteString(value)
			frame.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return out, errors.New("Qoder stream interrupted")
	}
	if frame.Len() > 0 {
		if err := flush(); err != nil {
			return out, err
		}
	}
	if !finished {
		return out, errors.New("Qoder stream ended without a finish reason")
	}
	if err := closeText(); err != nil {
		return out, err
	}
	if text.Len() > 0 {
		out.Content = append(out.Content, map[string]any{"type": "text", "text": text.String()})
	}
	switch finishReason {
	case "length":
		out.StopReason = "max_tokens"
	case "content_filter":
		out.StopReason = "refusal"
	case "stop", "tool_calls", "function_call":
		out.StopReason = "end_turn"
		if len(tools) > 0 {
			out.StopReason = "tool_use"
		} else if finishReason != "stop" {
			return out, errors.New("Qoder finished with missing tool calls")
		}
	default:
		return out, fmt.Errorf("unsupported Qoder finish reason: %s", finishReason)
	}
	if len(tools) > 0 && finishReason == "length" {
		return out, errors.New("Qoder tool call was truncated")
	}
	indices := make([]int, 0, len(tools))
	for index := range tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	firstTool := len(out.Content)
	for _, i := range indices {
		t := tools[i]
		var args map[string]any
		if t.Name == "" || json.Unmarshal([]byte(t.Args.String()), &args) != nil || args == nil {
			return out, errors.New("incomplete Qoder tool call")
		}
		if t.ID == "" {
			t.ID = "toolu_" + randomID()
		}
		block := map[string]any{"type": "tool_use", "id": t.ID, "name": t.Name, "input": args}
		out.Content = append(out.Content, block)
	}
	if emit != nil {
		for offset, i := range indices {
			index := firstTool + offset
			block := out.Content[index]
			if err := emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}}); err != nil {
				return out, err
			}
			if err := emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": tools[i].Args.String()}}); err != nil {
				return out, err
			}
			if err := emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return out, err
			}
		}
	}
	if emit != nil {
		if err := emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": out.StopReason, "stop_sequence": nil}, "usage": out.Usage}); err != nil {
			return out, err
		}
		if err := emit("message_stop", map[string]any{"type": "message_stop"}); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Qoder sometimes wraps a queued response in several JSON-encoded message strings.
func queueDelay(raw json.RawMessage, depth int) (int, bool) {
	if depth > 6 {
		return 0, false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	switch v := value.(type) {
	case string:
		return queueDelay(json.RawMessage(v), depth+1)
	case map[string]any:
		if v["isQueued"] == true {
			delay, _ := v["retryAfterSeconds"].(float64)
			return max(1, int(delay)), true
		}
		for _, key := range []string{"message", "body", "error", "data"} {
			if child, ok := v[key]; ok {
				b, _ := json.Marshal(child)
				if delay, queued := queueDelay(b, depth+1); queued {
					return delay, true
				}
			}
		}
	}
	return 0, false
}
