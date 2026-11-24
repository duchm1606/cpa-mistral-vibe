package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func modelIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Provider string
			Models   []struct{ ID string }
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Result.Provider != providerKey {
		t.Fatalf("unexpected response: %s", raw)
	}
	ids := make([]string, 0, len(env.Result.Models))
	for _, m := range env.Result.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

func fakeMistral(t *testing.T, catalog map[string][]string) *int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		ids, ok := catalog[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if r.URL.Path != "/v1/models" || !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		data := []map[string]any{{"id": "embed-only", "capabilities": map[string]any{"completion_chat": false}}}
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "capabilities": map[string]any{"completion_chat": true}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	oldBase, oldCache, oldState := mistralBase, modelsCache, loadConfig()
	mistralBase = srv.URL
	modelsCache = map[string]cachedModels{}
	storeConfig(pluginConfig{ConfigPath: t.TempDir() + "/missing.yaml"})
	t.Cleanup(func() {
		srv.Close()
		mistralBase, modelsCache = oldBase, oldCache
		storeConfig(oldState)
	})
	return &calls
}

func forAuthRequest(t *testing.T, storage string, attributes map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"AuthID":       "mistral-vibe-test",
		"AuthProvider": providerKey,
		"StorageJSON":  []byte(storage),
		"Attributes":   attributes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestModelsForAuthUsesCredentialKey(t *testing.T) {
	fakeMistral(t, map[string][]string{
		"mstrl_alpha": {"model-a1", "model-a2", "model-a3"},
		"mstrl_beta":  {"model-b1"},
	})

	fromAttributes := modelIDs(t, handleMethod("model.for_auth", forAuthRequest(t, "", map[string]string{"api_key": "mstrl_alpha"})))
	if got := strings.Join(fromAttributes, ","); got != "model-a1,model-a2,model-a3" {
		t.Fatalf("attributes key: got %s", got)
	}

	fromStorage := modelIDs(t, handleMethod("model.for_auth", forAuthRequest(t, `{"type":"mistral","api_key":"mstrl_beta"}`, nil)))
	if got := strings.Join(fromStorage, ","); got != "model-b1" {
		t.Fatalf("storage key: got %s", got)
	}
}

func TestModelsAreCachedPerKey(t *testing.T) {
	calls := fakeMistral(t, map[string][]string{
		"mstrl_alpha": {"model-a1"},
		"mstrl_beta":  {"model-b1"},
	})
	for i := 0; i < 3; i++ {
		for key, want := range map[string]string{"mstrl_alpha": "model-a1", "mstrl_beta": "model-b1"} {
			got := modelIDs(t, handleMethod("model.for_auth", forAuthRequest(t, "", map[string]string{"api_key": key})))
			if strings.Join(got, ",") != want {
				t.Fatalf("%s: got %v", key, got)
			}
		}
	}
	if *calls != 2 {
		t.Fatalf("expected one upstream call per key, got %d", *calls)
	}
}

func TestRequestFixesCoverModelsOfEveryCredential(t *testing.T) {
	fakeMistral(t, map[string][]string{
		"mstrl_alpha": {"model-a1"},
		"mstrl_beta":  {"model-b1"},
	})
	if isMistralModel("model-a1") {
		t.Fatal("model-a1 is not served before its credential is listed")
	}
	for _, key := range []string{"mstrl_alpha", "mstrl_beta"} {
		handleMethod("model.for_auth", forAuthRequest(t, "", map[string]string{"api_key": key}))
	}
	for _, id := range []string{"model-a1", "model-b1(high)", fallbackModels()[0].ID} {
		if !isMistralModel(id) {
			t.Fatalf("%s should be served", id)
		}
	}
	if isMistralModel("embed-only") || isMistralModel("gpt-5") {
		t.Fatal("models outside the chat catalog must stay untouched")
	}
}

func TestServesModelChecksCacheBeforeConfiguredKey(t *testing.T) {
	calls := fakeMistral(t, map[string][]string{
		"mstrl_alpha": {"model-a1"},
	})
	storeConfig(pluginConfig{APIKey: "mstrl_revoked", ConfigPath: t.TempDir() + "/missing.yaml"})

	handleMethod("model.for_auth", forAuthRequest(t, "", map[string]string{"api_key": "mstrl_alpha"}))
	if *calls != 1 {
		t.Fatalf("expected 1 discovery call, got %d", *calls)
	}
	if !isMistralModel("model-a1") || !isMistralModel(fallbackModels()[0].ID) {
		t.Fatal("cached and fallback models must still be served")
	}
	if isMistralModel("gpt-5") {
		t.Fatal("non-Mistral model must not be served")
	}
	if *calls != 1 {
		t.Fatalf("servesModel must not re-query configuredKey once modelsCache is populated, got %d calls", *calls)
	}
}

func TestModelsFallBackWithoutUsableKey(t *testing.T) {
	fakeMistral(t, map[string][]string{})
	want := make([]string, 0, 2)
	for _, m := range fallbackModels() {
		want = append(want, m.ID)
	}
	for name, raw := range map[string][]byte{
		"static without key":   handleMethod("model.static", nil),
		"register without key": handleMethod("model.register", nil),
		"rejected key":         handleMethod("model.for_auth", forAuthRequest(t, "", map[string]string{"api_key": "mstrl_revoked"})),
	} {
		if got := modelIDs(t, raw); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v", name, got)
		}
	}
}

func TestRegistrationDeclaresModelProvider(t *testing.T) {
	var env struct {
		Result struct {
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(registrationJSON(3), &env); err != nil {
		t.Fatal(err)
	}
	if !env.Result.Capabilities["model_provider"] {
		t.Fatal("model_provider capability is required for the host to call model.for_auth")
	}
}
