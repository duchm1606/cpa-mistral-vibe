package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// chatBody builds a chat-completions body shaped like the one CPA sends to
// Mistral for a Codex session that used view_image: every image is the only
// part of a tool message. The first message is a user turn with text and an
// image so mixed content is covered too.
func chatBody(t *testing.T, images int) []byte {
	t.Helper()
	msgs := []any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look at these"},
			imagePart(1),
		}},
	}
	for i := 2; i <= images; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": []any{imagePart(i)}},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "what is 2+2?"})
	b, err := json.Marshal(map[string]any{"model": "deepseek-v4-1-flash-1017", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func imagePart(n int) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": fmt.Sprintf("data:image/png;base64,IMG%02d", n)}}
}

// imagesIn returns the image numbers left in a body, oldest first, and how
// many omission notes it holds.
func imagesIn(t *testing.T, body []byte) (kept []int, notes int) {
	t.Helper()
	var doc struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	for _, m := range doc.Messages {
		parts, _ := m.Content.([]any)
		if m.Role == "tool" && len(parts) == 0 {
			t.Fatalf("tool message %s lost its content", m.ToolCallID)
		}
		for _, p := range parts {
			pm := p.(map[string]any)
			switch pm["type"] {
			case "image_url":
				var n int
				url := pm["image_url"].(map[string]any)["url"].(string)
				fmt.Sscanf(url[strings.LastIndex(url, "IMG")+3:], "%d", &n)
				kept = append(kept, n)
			case "text":
				if strings.HasPrefix(pm["text"].(string), "[image omitted") {
					notes++
				}
			}
		}
	}
	return kept, notes
}

func withConfig(t *testing.T, c pluginConfig) {
	t.Helper()
	old := loadConfig()
	storeConfig(c)
	t.Cleanup(func() { storeConfig(old) })
}

func TestParseConfigImageCap(t *testing.T) {
	cases := []struct {
		yaml  string
		strip bool
		max   int
	}{
		{"enabled: true\n", false, 20},
		{"enabled: true\nstrip-excess-images: true\n", true, 20},
		{"  strip-excess-images: \"yes\"\n  max-images: 15\n", true, 15},
		{"strip-excess-images: false\nmax-images: 0\n", false, 20},
		{"strip-excess-images: maybe\nmax-images: lots\n", false, 20},
	}
	for _, c := range cases {
		got := parseConfig([]byte(c.yaml))
		if got.StripExcessImages != c.strip || got.MaxImages != c.max {
			t.Errorf("parseConfig(%q) = strip %v max %d, want strip %v max %d", c.yaml, got.StripExcessImages, got.MaxImages, c.strip, c.max)
		}
	}
}

func TestImageCapOffByDefault(t *testing.T) {
	withConfig(t, defaultConfig())
	body := chatBody(t, 25)
	if out, changed := applyRequestFixes(body); changed {
		kept, _ := imagesIn(t, out)
		t.Fatalf("cap is off but the body changed; %d images left", len(kept))
	}
}

func TestImageCapDropsOldestInBlocks(t *testing.T) {
	withConfig(t, pluginConfig{StripExcessImages: true, MaxImages: 20})
	cases := []struct{ images, wantKept int }{
		{1, 1},
		{20, 20},
		{21, 11}, // drop a block of 10
		{30, 20}, // same block: the prefix has not moved since 21
		{31, 11}, // second block
		{45, 15},
	}
	for _, c := range cases {
		out, changed := applyRequestFixes(chatBody(t, c.images))
		if !changed {
			out = chatBody(t, c.images)
		}
		kept, notes := imagesIn(t, out)
		if len(kept) != c.wantKept || notes != c.images-c.wantKept {
			t.Errorf("%d images: kept %d with %d notes, want kept %d with %d notes", c.images, len(kept), notes, c.wantKept, c.images-c.wantKept)
			continue
		}
		for i, n := range kept {
			if want := c.images - c.wantKept + 1 + i; n != want {
				t.Errorf("%d images: kept %v, want the newest %d", c.images, kept, c.wantKept)
				break
			}
		}
	}
}

func TestImageCapIsIdempotent(t *testing.T) {
	withConfig(t, pluginConfig{StripExcessImages: true, MaxImages: 20})
	once, changed := applyRequestFixes(chatBody(t, 27))
	if !changed {
		t.Fatal("27 images with the cap on should change the body")
	}
	if _, again := applyRequestFixes(once); again {
		t.Fatal("a second pass changed an already capped body")
	}
}

func TestImageCapSmallLimits(t *testing.T) {
	for _, c := range []struct{ limit, images, wantKept int }{
		{1, 5, 1},  // block 1
		{2, 5, 2},  // block 1
		{3, 10, 3}, // block 1
		{4, 10, 4}, // block 2: drop 6
		{5, 10, 4}, // block 2: drop 5 rounds up to 6
	} {
		msgs := []any{}
		for i := 1; i <= c.images; i++ {
			msgs = append(msgs, map[string]any{"role": "user", "content": []any{imagePart(i)}})
		}
		stripExcessImages(msgs, c.limit)
		kept := 0
		forEachImagePart(msgs, func([]any, int) { kept++ })
		if kept != c.wantKept || kept > c.limit {
			t.Errorf("limit %d, %d images: kept %d, want %d", c.limit, c.images, kept, c.wantKept)
		}
	}
}
