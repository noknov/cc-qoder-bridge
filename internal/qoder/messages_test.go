package qoder

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolResultImagesPreserveParallelToolResults(t *testing.T) {
	var r Request
	raw := `{"model":"test-model","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"read-1","name":"Read","input":{"file_path":"a.png"}},{"type":"tool_use","id":"read-2","name":"Read","input":{"file_path":"b.png"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"read-1","content":[{"type":"text","text":"first image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]},{"type":"tool_result","tool_use_id":"read-2","content":[{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}]},{"type":"text","text":"Compare them."}]}]}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	body, err := Convert(r, Model{ID: "test-model", Config: map[string]any{"is_vl": true}})
	if err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected assistant, two tool results, then images; got %v", messages)
	}
	for i, id := range []string{"read-1", "read-2"} {
		m := messages[i+1].(map[string]any)
		if m["role"] != "tool" || m["tool_call_id"] != id {
			t.Fatalf("tool result order or ID lost: %v", m)
		}
	}
	if messages[1].(map[string]any)["content"] != "first image" {
		t.Fatal("tool text lost")
	}
	user := messages[3].(map[string]any)
	if user["role"] != "user" {
		t.Fatal(user)
	}
	parts := user["content"].([]any)
	if len(parts) != 5 {
		t.Fatalf("expected two labeled images followed by user text: %v", parts)
	}
	for i, expected := range map[int]string{1: "data:image/png;base64,aW1hZ2U=", 3: "https://example.com/b.png"} {
		image := parts[i].(map[string]any)
		if image["type"] != "image_url" || image["image_url"].(map[string]string)["url"] != expected {
			t.Fatal(image)
		}
	}
	if parts[4].(map[string]any)["text"] != "Compare them." {
		t.Fatal("user text lost")
	}
}

func TestToolResultImagesRespectVisionCapability(t *testing.T) {
	raw := json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]`)
	if _, _, err := toolResult(raw, Model{}); err == nil || !strings.Contains(err.Error(), "does not support images") {
		t.Fatalf("non-vision model accepted image: %v", err)
	}
}

func TestToolResultRejectsUnsupportedBlocksAndEmptyImages(t *testing.T) {
	model := Model{Config: map[string]any{"is_vl": true}}
	for _, raw := range []string{`[{"type":"document"}]`, `[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]`, `[{"type":"image","source":{"type":"url"}}]`} {
		if _, _, err := toolResult(json.RawMessage(raw), model); err == nil {
			t.Fatalf("invalid tool result accepted: %s", raw)
		}
	}
	text, images, err := toolResult(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`), model)
	if err != nil || text != "ab" || len(images) != 0 {
		t.Fatalf("text tool results regressed: %q %v %v", text, images, err)
	}
}
