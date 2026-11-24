package main

import (
	"encoding/json"
	"strings"
)

// Request-side fixes for Mistral, applied via the RequestInterceptor capability
// so users don't need host payload rules. STRICTLY scoped to this plugin's
// Mistral models: codex and other providers must not be touched (they rely on
// the developer role and accept reasoning_effort "medium", which Mistral does
// not).
//
//   - developer role        -> system        (Mistral rejects "developer")
//   - reasoning_effort medium -> high         (most Mistral models reject "medium")
//   - drop assistant reasoning_content         (Mistral 422s on it in history)
//   - optional, off by default: cap images per request (images.go)

type interceptRequest struct {
	ToFormat       string `json:"ToFormat"`
	Model          string `json:"Model"`
	RequestedModel string `json:"RequestedModel"`
	Body           []byte `json:"Body"`
}

// handleInterceptBefore runs before credential selection; nothing to do there.
func handleInterceptBefore(request []byte) []byte {
	return mustOK(map[string]any{})
}

// handleInterceptAfter runs after credential selection, when Body is the
// provider-bound (OpenAI chat-completions) request about to hit Mistral.
func handleInterceptAfter(request []byte) []byte {
	var req interceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return mustOK(map[string]any{})
	}
	if !isMistralModel(req.RequestedModel) && !isMistralModel(req.Model) {
		return mustOK(map[string]any{}) // not ours: keep untouched
	}
	newBody, changed := applyRequestFixes(req.Body)
	if !changed {
		return mustOK(map[string]any{})
	}
	return mustOK(map[string]any{"Body": newBody})
}

// handleRequestNormalize is the request.normalize RPC (request_normalizer
// capability). The host runs it AFTER translating the client payload into the
// provider format, so Body is the final OpenAI chat-completions request even
// when the client spoke the Responses API (Codex). request.intercept_after
// cannot cover that case: it sees the untranslated source payload (input[]),
// so a Responses "developer" item reached Mistral and was rejected with 422.
func handleRequestNormalize(request []byte) []byte {
	var req struct {
		FromFormat string
		ToFormat   string
		Model      string
		Stream     bool
		Body       []byte
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return mustOK(payloadResponse{})
	}
	if req.ToFormat != "openai" || !isMistralModel(req.Model) {
		return mustOK(payloadResponse{}) // not ours: keep untouched
	}
	newBody, changed := applyRequestFixes(req.Body)
	if !changed {
		return mustOK(payloadResponse{})
	}
	return mustOK(payloadResponse{Body: newBody})
}

// isMistralModel reports whether a model id is one this plugin serves.
func isMistralModel(model string) bool {
	m := baseModel(model)
	if m == "" {
		return false
	}
	return servesModel(m)
}

// baseModel strips CLIProxyAPI's thinking suffix, e.g. "model(high)".
func baseModel(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasSuffix(model, ")") {
		if i := strings.LastIndex(model, "("); i > 0 {
			model = strings.TrimSpace(model[:i])
		}
	}
	return model
}

// applyRequestFixes rewrites an OpenAI chat-completions body in place. Returns
// (body, false) when nothing matched so the host keeps the original bytes.
func applyRequestFixes(payload []byte) ([]byte, bool) {
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, false
	}
	changed := false
	if msgs, ok := doc["messages"].([]any); ok {
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if mm == nil {
				continue
			}
			switch role, _ := mm["role"].(string); role {
			case "developer":
				mm["role"] = "system"
				changed = true
			case "assistant":
				if _, ok := mm["reasoning_content"]; ok {
					delete(mm, "reasoning_content")
					changed = true
				}
			}
		}
		if cfg := loadConfig(); cfg.StripExcessImages && stripExcessImages(msgs, cfg.MaxImages) {
			changed = true
		}
	}
	if eff, _ := doc["reasoning_effort"].(string); eff == "medium" {
		doc["reasoning_effort"] = "high"
		changed = true
	}
	// Responses-API source payload (Codex): reasoning.effort is read by the
	// host's thinking pipeline, which applies it AFTER request.normalize, so it
	// must be rewritten here in the untranslated body (request.intercept_after).
	if r, ok := doc["reasoning"].(map[string]any); ok {
		if eff, _ := r["effort"].(string); eff == "medium" {
			r["effort"] = "high"
			changed = true
		}
	}
	// Responses-API input items: developer -> system, mirroring messages[].
	if items, ok := doc["input"].([]any); ok {
		for _, it := range items {
			if m, _ := it.(map[string]any); m != nil {
				if role, _ := m["role"].(string); role == "developer" {
					m["role"] = "system"
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}
