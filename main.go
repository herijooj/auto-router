package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

type HealthStatus struct {
	Status string `json:"status"`
	Model  string `json:"model,omitempty"`
}

type LlamaSwapEvent struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type ModelRuntimeStatus struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type AutoRouter struct {
	config       Config
	client       *http.Client
	loadedModels map[string]bool
	loadedMutex  sync.RWMutex
	currentModel string
	currentMutex sync.RWMutex
}

type Config struct {
	LlamaSwapURL        string
	ListenAddr          string
	ExcludeModels       []string
	PreferredModels     []string
	HealthCheckInterval int
}

var alwaysExcludedModels = []string{"glm-ocr"}

func main() {
	config := loadConfig()
	ar := &AutoRouter{
		config:       config,
		client:       &http.Client{Timeout: 10 * time.Second},
		loadedModels: make(map[string]bool),
	}

	go ar.healthCheckLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", ar.handleHealth)
	mux.HandleFunc("/v1/models", ar.handleModels)
	mux.HandleFunc("/v1/chat/completions", ar.handleChatCompletions)

	log.Printf("Auto router starting on %s", config.ListenAddr)
	log.Fatal(http.ListenAndServe(config.ListenAddr, mux))
}

func loadConfig() Config {
	defaultExcludeModels := []string{"lfm2-350m", "glm-ocr"}
	defaultPreferredModels := []string{"qwen3.5-9b-jackrong", "qwen3.5-9b", "qwen3.5-4b-jackrong", "qwen3.5-4b", "jan-code-4b"}
	excludeModels := parseCommaSeparatedList(getEnv("EXCLUDE_MODELS", strings.Join(defaultExcludeModels, ",")))
	excludeModels = ensureModelsExcluded(excludeModels, alwaysExcludedModels)

	return Config{
		LlamaSwapURL:        getEnv("LLAMA_SWAP_URL", "http://127.0.0.1:9292"),
		ListenAddr:          getEnv("LISTEN_ADDR", ":9293"),
		ExcludeModels:       excludeModels,
		PreferredModels:     parseCommaSeparatedList(getEnv("PREFERRED_MODELS", strings.Join(defaultPreferredModels, ","))),
		HealthCheckInterval: getEnvInt("HEALTH_CHECK_INTERVAL", 10),
	}
}

func ensureModelsExcluded(models []string, required []string) []string {
	seen := make(map[string]struct{}, len(models)+len(required))
	result := make([]string, 0, len(models)+len(required))

	for _, model := range models {
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}

	for _, model := range required {
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}

	return result
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultValue
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return defaultValue
	}

	return value
}

func parseCommaSeparatedList(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))

	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}

	return result
}

func (ar *AutoRouter) healthCheckLoop() {
	ticker := time.NewTicker(time.Duration(ar.config.HealthCheckInterval) * time.Second)
	defer ticker.Stop()

	ar.checkModelHealth()

	for range ticker.C {
		ar.checkModelHealth()
	}
}

func (ar *AutoRouter) checkModelHealth() {
	loadedModels, err := ar.getLoadedModels()
	if err != nil {
		log.Printf("Failed to get loaded models: %v", err)
		ar.loadedMutex.Lock()
		ar.loadedModels = make(map[string]bool)
		ar.loadedMutex.Unlock()

		ar.currentMutex.Lock()
		ar.currentModel = ""
		ar.currentMutex.Unlock()
		return
	}

	ar.loadedMutex.Lock()
	defer ar.loadedMutex.Unlock()

	ar.loadedModels = loadedModels

	ar.currentMutex.Lock()
	if ar.currentModel != "" && !ar.loadedModels[ar.currentModel] {
		ar.currentModel = ""
	}
	ar.currentMutex.Unlock()
}

func (ar *AutoRouter) getLoadedModels() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ar.config.LlamaSwapURL+"/api/events", nil)
	if err != nil {
		return nil, err
	}

	resp, err := ar.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code from /api/events: %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}

		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			if errors.Is(err, io.EOF) {
				break
			}
			continue
		}

		rawEvent := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if rawEvent == "" {
			continue
		}

		var event LlamaSwapEvent
		if err := json.Unmarshal([]byte(rawEvent), &event); err != nil {
			continue
		}

		if event.Type != "modelStatus" || event.Data == "" {
			if errors.Is(err, io.EOF) {
				break
			}
			continue
		}

		var runtimeStatuses []ModelRuntimeStatus
		if err := json.Unmarshal([]byte(event.Data), &runtimeStatuses); err != nil {
			return nil, fmt.Errorf("failed to parse modelStatus event: %w", err)
		}

		loaded := make(map[string]bool)
		for _, model := range runtimeStatuses {
			if model.State != "ready" || ar.isExcluded(model.ID) {
				continue
			}
			loaded[model.ID] = true
		}

		return loaded, nil
	}

	return nil, errors.New("did not receive modelStatus from /api/events")
}

func (ar *AutoRouter) getModels() (*ModelsResponse, error) {
	resp, err := ar.client.Get(ar.config.LlamaSwapURL + "/v1/models")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var models ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, err
	}

	return &models, nil
}

func (ar *AutoRouter) isExcluded(modelID string) bool {
	for _, excluded := range ar.config.ExcludeModels {
		if modelID == excluded {
			return true
		}
	}
	return false
}

func (ar *AutoRouter) getPreferredModel() string {
	for _, preferred := range ar.config.PreferredModels {
		ar.loadedMutex.RLock()
		loaded := ar.loadedModels[preferred]
		ar.loadedMutex.RUnlock()

		if loaded {
			return preferred
		}
	}
	return ""
}

func (ar *AutoRouter) getAnyLoadedModel() string {
	ar.loadedMutex.RLock()
	defer ar.loadedMutex.RUnlock()

	if len(ar.loadedModels) == 0 {
		return ""
	}

	models := make([]string, 0, len(ar.loadedModels))
	for model := range ar.loadedModels {
		models = append(models, model)
	}
	sort.Strings(models)
	return models[0]
}

func (ar *AutoRouter) isModelLoaded(modelID string) bool {
	ar.loadedMutex.RLock()
	defer ar.loadedMutex.RUnlock()
	return ar.loadedModels[modelID]
}

func (ar *AutoRouter) getFirstAvailableModel() string {
	model := ar.getPreferredModel()
	if model != "" {
		ar.setCurrentModel(model)
		return model
	}

	model = ar.getCurrentModel()
	if model != "" && ar.isModelLoaded(model) {
		return model
	}

	model = ar.getAnyLoadedModel()
	if model != "" {
		ar.setCurrentModel(model)
		return model
	}

	return ar.loadPreferredModel()
}

func (ar *AutoRouter) loadPreferredModel() string {
	for _, preferred := range ar.config.PreferredModels {
		log.Printf("Attempting to load model: %s", preferred)

		resp, err := ar.client.Post(
			ar.config.LlamaSwapURL+"/v1/chat/completions",
			"application/json",
			bytes.NewBufferString(fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`, preferred)),
		)
		if err != nil {
			log.Printf("Failed to request model %s: %v", preferred, err)
			continue
		}
		resp.Body.Close()

		if ar.waitForModelReady(preferred, 60*time.Second) {
			ar.setCurrentModel(preferred)
			return preferred
		}
	}

	return ""
}

func (ar *AutoRouter) waitForModelReady(modelID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		ar.checkModelHealth()
		if ar.isModelLoaded(modelID) {
			return true
		}
		time.Sleep(1500 * time.Millisecond)
	}

	return false
}

func (ar *AutoRouter) setCurrentModel(modelID string) {
	ar.currentMutex.Lock()
	defer ar.currentMutex.Unlock()
	ar.currentModel = modelID
}

func (ar *AutoRouter) getCurrentModel() string {
	ar.currentMutex.RLock()
	defer ar.currentMutex.RUnlock()
	return ar.currentModel
}

func (ar *AutoRouter) handleHealth(w http.ResponseWriter, r *http.Request) {
	ar.loadedMutex.RLock()
	loadedCount := len(ar.loadedModels)
	ar.loadedMutex.RUnlock()

	status := HealthStatus{
		Status: "ok",
		Model:  ar.getCurrentModel(),
	}

	if loadedCount > 0 {
		status.Status = "ok"
	} else {
		status.Status = "no models loaded"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (ar *AutoRouter) handleModels(w http.ResponseWriter, r *http.Request) {
	models := ModelsResponse{
		Object: "list",
		Data: []Model{
			{
				ID:      "auto",
				Name:    "Auto Router",
				Object:  "model",
				OwnedBy: "llama-auto-router",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

func (ar *AutoRouter) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	modelValue, ok := req["model"]
	if !ok {
		http.Error(w, "Missing model field", http.StatusBadRequest)
		return
	}

	requestedModel, ok := modelValue.(string)
	if !ok || requestedModel == "" {
		http.Error(w, "Invalid model field", http.StatusBadRequest)
		return
	}

	if requestedModel != "auto" {
		ar.proxyRequest(w, r, req)
		return
	}

	ar.checkModelHealth()

	modelID := ar.getFirstAvailableModel()
	if modelID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "No models available",
			"message": "No models are currently loaded and unable to load a preferred model",
		})
		return
	}

	log.Printf("Routing request to model: %s", modelID)
	req["model"] = modelID

	ar.proxyRequest(w, r, req)
}

func (ar *AutoRouter) proxyRequest(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	targetURL, err := url.Parse(ar.config.LlamaSwapURL)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetURL.Host
		req.Header.Set("X-Forwarded-Host", r.Host)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	proxy.ServeHTTP(w, r)
}
