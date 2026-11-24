package main

import (
	"encoding/json"
	"sync"
	"time"
)

const mistralBaseURL = "https://api.mistral.ai/v1"

type modelCaps struct {
	CompletionChat bool `json:"completion_chat"`
	Reasoning      bool `json:"reasoning"`
	Vision         bool `json:"vision"`
}

type mistralModel struct {
	ID           string    `json:"id"`
	Capabilities modelCaps `json:"capabilities"`
}

type modelsListResp struct {
	Data []mistralModel `json:"data"`
}

type cachedModels struct {
	models []mistralModel
	at     time.Time
}

var (
	modelsCacheMu sync.Mutex
	modelsCache   = map[string]cachedModels{}
)

const modelsCacheTTL = 10 * time.Minute

func discoverModels(key string) []mistralModel {
	if key == "" {
		return fallbackModels()
	}
	modelsCacheMu.Lock()
	cached, ok := modelsCache[key]
	modelsCacheMu.Unlock()
	if ok && time.Since(cached.at) < modelsCacheTTL {
		return cached.models
	}

	var out modelsListResp
	if err := mistralGet(key, "/v1/models", &out); err != nil {
		return fallbackModels()
	}
	chat := make([]mistralModel, 0, len(out.Data))
	for _, m := range out.Data {
		if m.Capabilities.CompletionChat {
			chat = append(chat, m)
		}
	}
	if len(chat) == 0 {
		return fallbackModels()
	}
	modelsCacheMu.Lock()
	modelsCache[key] = cachedModels{models: chat, at: time.Now()}
	modelsCacheMu.Unlock()
	return chat
}

func servesModel(id string) bool {
	modelsCacheMu.Lock()
	hasCached := len(modelsCache) > 0
	for _, cached := range modelsCache {
		for _, m := range cached.models {
			if m.ID == id {
				modelsCacheMu.Unlock()
				return true
			}
		}
	}
	modelsCacheMu.Unlock()

	for _, m := range fallbackModels() {
		if m.ID == id {
			return true
		}
	}
	if hasCached {
		return false
	}
	for _, m := range discoverModels(configuredKey()) {
		if m.ID == id {
			return true
		}
	}
	return false
}

func configuredKey() string {
	key, err := resolveKey()
	if err != nil {
		return ""
	}
	return key
}

func authModelKey(request []byte) string {
	var req struct {
		StorageJSON []byte            `json:"StorageJSON"`
		Attributes  map[string]string `json:"Attributes"`
	}
	_ = json.Unmarshal(request, &req)
	if m := keyPattern.FindString(req.Attributes["api_key"]); m != "" {
		return m
	}
	if m := keyPattern.Find(req.StorageJSON); m != nil {
		return string(m)
	}
	return configuredKey()
}

func fallbackModels() []mistralModel {
	return []mistralModel{
		{ID: "deepseek-v4-1-flash-1017", Capabilities: modelCaps{CompletionChat: true, Reasoning: true}},
		{ID: "zai-glm-5-3-flash", Capabilities: modelCaps{CompletionChat: true, Reasoning: true}},
	}
}

func modelsJSON(key string) []byte {
	src := discoverModels(key)
	models := make([]map[string]any, 0, len(src))
	for _, m := range src {
		entry := map[string]any{
			"ID":          m.ID, // native id, sent upstream as-is
			"Name":        m.ID,
			"Object":      "model",
			"OwnedBy":     "mistral",
			"DisplayName": m.ID,
			"Type":        "chat",
		}
		if m.Capabilities.Reasoning {
			entry["Thinking"] = map[string]any{
				"Min":            0,
				"Max":            128000,
				"ZeroAllowed":    true,
				"DynamicAllowed": true,
				"Levels":         []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
			}
		}
		models = append(models, entry)
	}
	return mustOK(map[string]any{"Provider": providerKey, "Models": models})
}
