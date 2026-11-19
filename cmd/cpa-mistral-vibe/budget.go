package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConfigPath = "/app/config.yaml"
	mistralBase       = "https://api.mistral.ai"
)

var keyPattern = regexp.MustCompile(`mstrl_[A-Za-z0-9_-]+`)

type pluginConfig struct {
	APIKey     string
	ConfigPath string
	RawYAML    []byte
	// StripExcessImages turns on the image cap (images.go). Off by default.
	StripExcessImages bool
	// MaxImages is the most images a request may carry when the cap is on.
	MaxImages int
}

// defaultMaxImages is Mistral's limit, measured 2026-11-19 on
// deepseek-v4-1-flash-1017: 21 images in one request (one message or spread
// over turns) fails with 400 code 3051 "Total number of images exceeds the
// maximum allowed of 20."
const defaultMaxImages = 20

func defaultConfig() pluginConfig {
	return pluginConfig{ConfigPath: defaultConfigPath, MaxImages: defaultMaxImages}
}

func storeConfig(c pluginConfig) {
	stateMu.Lock()
	state = c
	stateMu.Unlock()
}

func loadConfig() pluginConfig {
	stateMu.Lock()
	defer stateMu.Unlock()
	return state
}

// parseConfig reads the plugin's own config block. Only a few optional scalar
// fields matter here, so a line scan is enough and avoids a YAML dependency.
func parseConfig(configYAML []byte) pluginConfig {
	c := defaultConfig()
	c.RawYAML = configYAML
	for _, line := range strings.Split(string(configYAML), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "api-key:"):
			if v := scalarValue(trimmed[len("api-key:"):]); v != "" {
				c.APIKey = v
			}
		case strings.HasPrefix(trimmed, "config-path:"):
			if v := scalarValue(trimmed[len("config-path:"):]); v != "" {
				c.ConfigPath = v
			}
		case strings.HasPrefix(trimmed, "strip-excess-images:"):
			switch strings.ToLower(scalarValue(trimmed[len("strip-excess-images:"):])) {
			case "true", "yes", "on", "1":
				c.StripExcessImages = true
			}
		case strings.HasPrefix(trimmed, "max-images:"):
			if n, err := strconv.Atoi(scalarValue(trimmed[len("max-images:"):])); err == nil && n > 0 {
				c.MaxImages = n
			}
		}
	}
	if c.ConfigPath == "" {
		c.ConfigPath = defaultConfigPath
	}
	return c
}

func scalarValue(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	return strings.TrimSpace(s)
}

// resolveKey returns the Mistral key, preferring an explicit api-key, then any
// mstrl_ token in the plugin's own config block, then the CPA config file.
func resolveKey() (string, error) {
	c := loadConfig()
	if k := strings.TrimSpace(c.APIKey); k != "" {
		return k, nil
	}
	if m := keyPattern.Find(c.RawYAML); m != nil {
		return string(m), nil
	}
	path := c.ConfigPath
	if path == "" {
		path = defaultConfigPath
	}
	if data, err := os.ReadFile(path); err == nil {
		if m := keyPattern.Find(data); m != nil {
			return string(m), nil
		}
	}
	// Last resort: a stored Mistral credential file in the auth directory.
	for _, dir := range []string{"/home/cliproxy/.cli-proxy-api", "/app/data"} {
		matches, _ := filepath.Glob(filepath.Join(dir, "mistral-*.json"))
		for _, f := range matches {
			if b, err := os.ReadFile(f); err == nil {
				if m := keyPattern.Find(b); m != nil {
					return string(m), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no Mistral key (mstrl_...) found")
}

type budgetPool struct {
	UsagePercentage float64 `json:"usage_percentage"`
	InitialBudget   float64 `json:"initial_budget"`
	Currency        string  `json:"currency"`
	ResetAt         string  `json:"reset_at"`
	PaygEnabled     bool    `json:"payg_enabled"`
}

func (p budgetPool) used() float64      { return p.InitialBudget * p.UsagePercentage / 100 }
func (p budgetPool) remaining() float64 { return p.InitialBudget - p.used() }

type budgetResponse struct {
	VibeBudget budgetPool `json:"vibe_budget"`
	APIBudget  budgetPool `json:"api_budget"`
}

type subResponse struct {
	PlanName       string `json:"plan_name"`
	PaygEnabled    bool   `json:"payg_enabled"`
	VibePaygEnable bool   `json:"vibe_payg_enabled"`
	Enterprise     bool   `json:"enterprise"`
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func mistralGet(key, path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, mistralBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mistral-usage-plugin/"+pluginVersion)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, path, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

func fetchBudget(key string) (*budgetResponse, error) {
	var b budgetResponse
	if err := mistralGet(key, "/v1/billing/budget", &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func fetchSubscription(key string) *subResponse {
	var s subResponse
	if err := mistralGet(key, "/v1/billing/subscription", &s); err != nil {
		return nil
	}
	return &s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
