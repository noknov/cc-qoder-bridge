package qoder

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Model struct {
	ID     string         `json:"id"`
	Name   string         `json:"display_name"`
	Config map[string]any `json:"-"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type Request struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	System   json.RawMessage `json:"system"`
	Tools    []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Schema      map[string]any `json:"input_schema"`
	} `json:"tools"`
	MaxTokens     int             `json:"max_tokens"`
	Stream        bool            `json:"stream"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	StopSequences []string        `json:"stop_sequences"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	Metadata      struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Source    struct {
		Type, URL, Data string
		MediaType       string `json:"media_type"`
	} `json:"source"`
}

func blocks(raw json.RawMessage) ([]block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []block{{Type: "text", Text: text}}, nil
	}
	var out []block
	if json.Unmarshal(raw, &out) != nil {
		return nil, errors.New("content must be a string or content block array")
	}
	return out, nil
}

func textOnly(raw json.RawMessage) (string, error) {
	parts, err := blocks(raw)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("unsupported text content block: %s", p.Type)
		}
		text.WriteString(p.Text)
	}
	return text.String(), nil
}

func imagePart(p block, model Model) (any, error) {
	if model.Config["is_vl"] != true {
		return nil, errors.New("selected model does not support images")
	}
	u := p.Source.URL
	if p.Source.Type == "base64" {
		if p.Source.MediaType == "" || p.Source.Data == "" {
			return nil, errors.New("image source is empty")
		}
		u = "data:" + p.Source.MediaType + ";base64," + p.Source.Data
	}
	if u == "" {
		return nil, errors.New("image source is empty")
	}
	return map[string]any{"type": "image_url", "image_url": map[string]string{"url": u}}, nil
}

// Qoder tool messages accept text; their images follow in a user message.
// Keep all parallel tool results together before appending their image content.
func toolResult(raw json.RawMessage, model Model) (string, []any, error) {
	parts, err := blocks(raw)
	if err != nil {
		return "", nil, err
	}
	var text strings.Builder
	var images []any
	for _, p := range parts {
		switch p.Type {
		case "text":
			text.WriteString(p.Text)
		case "image":
			image, err := imagePart(p, model)
			if err != nil {
				return "", nil, err
			}
			images = append(images, image)
		default:
			return "", nil, fmt.Errorf("unsupported tool result content block: %s", p.Type)
		}
	}
	if text.Len() == 0 && len(images) > 0 {
		text.WriteString("Image attached in the following user message.")
	}
	return text.String(), images, nil
}

func Convert(r Request, model Model) (map[string]any, error) {
	if len(r.Messages) == 0 {
		return nil, errors.New("messages must not be empty")
	}
	if r.MaxTokens <= 0 {
		return nil, errors.New("max_tokens must be positive")
	}
	// Unsupported controls are rejected rather than silently changing generation semantics.
	if len(r.ToolChoice) > 0 && string(r.ToolChoice) != "null" {
		var tc struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(r.ToolChoice, &tc) != nil || tc.Type != "auto" {
			return nil, errors.New("only tool_choice auto is supported")
		}
	}
	if len(r.StopSequences) > 0 {
		return nil, errors.New("stop_sequences are not supported")
	}
	if r.Temperature != nil || r.TopP != nil {
		return nil, errors.New("temperature and top_p are not supported")
	}
	messages := []any{}
	if sys, err := textOnly(r.System); err != nil {
		return nil, err
	} else if sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	lastUser := ""
	for _, m := range r.Messages {
		parts, err := blocks(m.Content)
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case "user", "system":
			var content, toolImages, results []any
			for _, p := range parts {
				switch p.Type {
				case "text":
					content = append(content, map[string]any{"type": "text", "text": p.Text})
					if m.Role == "user" {
						lastUser = p.Text
					}
				case "image":
					image, err := imagePart(p, model)
					if err != nil {
						return nil, err
					}
					content = append(content, image)
				case "tool_result":
					if p.ToolUseID == "" {
						return nil, errors.New("tool_result requires tool_use_id")
					}
					text, images, err := toolResult(p.Content, model)
					if err != nil {
						return nil, err
					}
					results = append(results, map[string]any{"role": "tool", "tool_call_id": p.ToolUseID, "content": text})
					if len(images) > 0 {
						toolImages = append(toolImages, map[string]any{"type": "text", "text": "Images from tool " + p.ToolUseID + ":"})
						toolImages = append(toolImages, images...)
					}
				default:
					return nil, fmt.Errorf("unsupported user content block: %s", p.Type)
				}
			}
			messages = append(messages, results...)
			content = append(toolImages, content...)
			if len(content) > 0 {
				messages = append(messages, map[string]any{"role": m.Role, "content": content})
			}
		case "assistant":
			var text strings.Builder
			calls := []any{}
			for _, p := range parts {
				switch p.Type {
				case "text":
					text.WriteString(p.Text)
				case "thinking", "redacted_thinking": // Provider-specific thinking signatures cannot be replayed to Qoder.
				case "tool_use":
					if p.ID == "" || p.Name == "" {
						return nil, errors.New("tool_use requires id and name")
					}
					if p.Input == nil {
						p.Input = map[string]any{}
					}
					args, _ := json.Marshal(p.Input)
					calls = append(calls, map[string]any{"id": p.ID, "type": "function", "function": map[string]string{"name": p.Name, "arguments": string(args)}})
				default:
					return nil, fmt.Errorf("unsupported assistant content block: %s", p.Type)
				}
			}
			out := map[string]any{"role": "assistant", "content": text.String()}
			if len(calls) > 0 {
				out["tool_calls"] = calls
				if text.Len() == 0 {
					out["content"] = " "
				}
			}
			messages = append(messages, out)
		default:
			return nil, fmt.Errorf("unsupported role: %s", m.Role)
		}
	}
	tools := []any{}
	for _, t := range r.Tools {
		if t.Name == "" || t.Schema == nil {
			return nil, errors.New("tools require name and input_schema")
		}
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}})
	}
	config := make(map[string]any, len(model.Config))
	for k, v := range model.Config {
		config[k] = v
	}
	config["key"] = model.ID
	maxTokens := r.MaxTokens
	if limit, ok := config["max_output_tokens"].(float64); ok && limit > 0 && maxTokens > int(limit) {
		maxTokens = int(limit)
	}
	config["max_output_tokens"] = maxTokens
	record, session := randomID(), r.Metadata.UserID
	if session == "" {
		session = randomID()
	}
	return map[string]any{
		"request_id": randomID(), "request_set_id": record, "chat_record_id": record, "session_id": session,
		"stream": true, "chat_task": "FREE_INPUT", "is_reply": true, "is_retry": false,
		"source": 1, "version": "3", "session_type": "qodercli", "agent_id": "agent_common", "task_id": "common",
		"code_language": "", "chat_prompt": "", "image_urls": nil, "aliyun_user_type": "", "system": "",
		"messages": messages, "tools": tools, "parameters": map[string]int{"max_tokens": maxTokens}, "model_config": config,
		"chat_context": map[string]any{"chatPrompt": "", "imageUrls": nil, "features": []any{}, "text": lastUser,
			"extra": map[string]any{"context": []any{}, "modelConfig": map[string]any{"key": model.ID, "is_reasoning": config["is_reasoning"]}, "originalContent": lastUser}},
		"business": map[string]any{"product": "cli", "version": "1.0.0", "type": "agent", "stage": "start", "id": randomID(), "name": string([]rune(lastUser)[:min(30, len([]rune(lastUser)))]), "begin_at": time.Now().UnixMilli()},
	}, nil
}
