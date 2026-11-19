package main

import "fmt"

// Optional image cap (config: strip-excess-images, max-images).
//
// Mistral counts every image in the request, across all turns, and rejects the
// whole request once the total passes its limit (20 for
// deepseek-v4-1-flash-1017). Clients resend their full history every turn, so
// a single image too many breaks the conversation for good: every later turn
// fails the same way, even a text-only one.
//
// When the cap is on, the oldest images in the outgoing request are swapped for
// a short text note. Only the copy sent to Mistral changes: the client keeps
// its full history, and each request is counted again from scratch.
//
// Images are dropped in blocks of half the limit rather than one at a time.
// The number dropped then depends only on the image count, and the start of
// the prompt stays the same until the count reaches the next block. Dropping
// exactly one image per new image would change the prompt prefix every turn.
//
// The note is a text part in the same message, never a removed message: a
// tool message whose only content was an image must stay, or its tool call is
// left without a result.

// stripExcessImages caps the image_url parts in OpenAI chat-completions
// messages at limit, replacing the oldest ones in place. It reports whether
// anything changed. Running it again on its own output changes nothing.
func stripExcessImages(msgs []any, limit int) bool {
	if limit <= 0 {
		limit = defaultMaxImages
	}
	total := 0
	forEachImagePart(msgs, func([]any, int) { total++ })
	if total <= limit {
		return false
	}

	block := limit / 2
	if block < 1 {
		block = 1
	}
	drop := (total - limit + block - 1) / block * block
	if drop > total {
		drop = total
	}

	note := map[string]any{
		"type": "text",
		"text": fmt.Sprintf("[image omitted: the upstream model accepts at most %d images per request, so this older image was dropped]", limit),
	}
	dropped := 0
	forEachImagePart(msgs, func(parts []any, i int) {
		if dropped < drop {
			parts[i] = note
			dropped++
		}
	})
	return true
}

// forEachImagePart calls fn for every image_url part, oldest first. fn may
// replace parts[i].
func forEachImagePart(msgs []any, fn func(parts []any, i int)) {
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		parts, _ := mm["content"].([]any)
		for i, p := range parts {
			if pm, _ := p.(map[string]any); pm != nil && pm["type"] == "image_url" {
				fn(parts, i)
			}
		}
	}
}
