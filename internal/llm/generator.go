package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wingitman/lup/internal/config"
)

// NewGenerator creates a text generator for the selected agent profile.
func NewGenerator(agent config.AgentConfig, level string) *GeneratorClient {
	if agent.TimeoutSecs <= 0 {
		agent.TimeoutSecs = 120
	}
	return &GeneratorClient{
		agent: agent,
		level: level,
		http:  &http.Client{Timeout: time.Duration(agent.TimeoutSecs) * time.Second},
	}
}

// GeneratorClient supports direct model APIs and agent-backed generation APIs.
type GeneratorClient struct {
	agent config.AgentConfig
	level string
	http  *http.Client
}

// Complete sends one summarisation request and returns assistant text.
func (g *GeneratorClient) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	switch strings.ToLower(g.agent.Provider) {
	case "", "openai_compatible":
		baseURL := g.agent.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		return New(baseURL, g.agent.ResolvedAPIKey(), g.agent.Model, g.agent.EmbedModel, g.agent.TimeoutSecs).Complete(ctx, systemPrompt, userPrompt)
	case "opencode_zen":
		return g.completeOpenCodeZen(ctx, systemPrompt, userPrompt)
	case "openai_responses":
		baseURL := strings.TrimRight(g.agent.BaseURL, "/")
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		return g.completeResponses(ctx, baseURL+"/responses", g.agent.Model, g.agent.ResolvedAPIKey(), systemPrompt, userPrompt)
	case "anthropic_messages":
		baseURL := strings.TrimRight(g.agent.BaseURL, "/")
		if baseURL == "" {
			baseURL = "https://api.anthropic.com/v1"
		}
		return g.completeMessages(ctx, baseURL+"/messages", g.agent.Model, g.agent.ResolvedAPIKey(), systemPrompt, userPrompt)
	case "cursor_agent":
		return g.completeCursorAgent(ctx, systemPrompt, userPrompt)
	default:
		return "", fmt.Errorf("unknown generator provider %q", g.agent.Provider)
	}
}

func (g *GeneratorClient) completeOpenCodeZen(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	model := strings.TrimPrefix(g.agent.Model, "opencode/")
	key := g.agent.ResolvedAPIKey()
	if key == "" {
		return "", fmt.Errorf("opencode_zen requires api_key or api_key_env")
	}

	switch {
	case strings.HasPrefix(model, "gpt-"):
		return g.completeResponses(ctx, "https://opencode.ai/zen/v1/responses", model, key, systemPrompt, userPrompt)
	case strings.Contains(model, "claude") || strings.Contains(model, "qwen"):
		return g.completeMessages(ctx, "https://opencode.ai/zen/v1/messages", model, key, systemPrompt, userPrompt)
	default:
		return g.completeChat(ctx, "https://opencode.ai/zen/v1/chat/completions", model, key, systemPrompt, userPrompt)
	}
}

func (g *GeneratorClient) completeChat(ctx context.Context, endpoint, model, apiKey, systemPrompt, userPrompt string) (string, error) {
	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.2,
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *apiError `json:"error,omitempty"`
	}
	if err := g.postJSON(ctx, endpoint, apiKey, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("llm: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: empty response from model")
	}
	return out.Choices[0].Message.Content, nil
}

func (g *GeneratorClient) completeResponses(ctx context.Context, endpoint, model, apiKey, systemPrompt, userPrompt string) (string, error) {
	body := map[string]interface{}{
		"model": model,
		"input": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
	}
	addReasoning(body, g.level)
	var out struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Text string `json:"text"`
				Type string `json:"type"`
			} `json:"content"`
		} `json:"output"`
		Error *apiError `json:"error,omitempty"`
	}
	if err := g.postJSON(ctx, endpoint, apiKey, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("llm: %s", out.Error.Message)
	}
	if out.OutputText != "" {
		return out.OutputText, nil
	}
	var b strings.Builder
	for _, item := range out.Output {
		for _, content := range item.Content {
			b.WriteString(content.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("llm: empty response from model")
	}
	return b.String(), nil
}

func (g *GeneratorClient) completeMessages(ctx context.Context, endpoint, model, apiKey, systemPrompt, userPrompt string) (string, error) {
	body := map[string]interface{}{
		"model":      model,
		"system":     systemPrompt,
		"max_tokens": 4096,
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
			Type string `json:"type"`
		} `json:"content"`
		Error *apiError `json:"error,omitempty"`
	}
	if err := g.postJSON(ctx, endpoint, apiKey, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("llm: %s", out.Error.Message)
	}
	var b strings.Builder
	for _, content := range out.Content {
		b.WriteString(content.Text)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("llm: empty response from model")
	}
	return b.String(), nil
}

func (g *GeneratorClient) completeCursorAgent(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	key := g.agent.ResolvedAPIKey()
	if key == "" {
		return "", fmt.Errorf("cursor_agent requires api_key or api_key_env")
	}
	baseURL := strings.TrimRight(g.agent.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cursor.com/v1"
	}
	prompt := systemPrompt + "\n\n" + userPrompt + "\n\nDo not modify files. Return only the requested JSON in the final result."
	body := map[string]interface{}{
		"prompt": map[string]string{"text": prompt},
		"mode":   cursorMode(g.agent.Mode),
	}
	if g.agent.Model != "" {
		body["model"] = map[string]interface{}{
			"id":     g.agent.Model,
			"params": cursorParams(g.level),
		}
	}
	var created struct {
		Agent struct {
			ID          string `json:"id"`
			LatestRunID string `json:"latestRunId"`
		} `json:"agent"`
		Run struct {
			ID      string `json:"id"`
			AgentID string `json:"agentId"`
			Status  string `json:"status"`
			Result  string `json:"result"`
		} `json:"run"`
	}
	if err := g.postCursorJSON(ctx, baseURL+"/agents", key, body, &created); err != nil {
		return "", err
	}
	agentID := created.Run.AgentID
	if agentID == "" {
		agentID = created.Agent.ID
	}
	runID := created.Run.ID
	if runID == "" {
		runID = created.Agent.LatestRunID
	}
	if agentID == "" || runID == "" {
		return "", fmt.Errorf("cursor_agent: create response missing agent/run id")
	}

	deadline := time.Now().Add(time.Duration(g.agent.TimeoutSecs) * time.Second)
	for {
		var run struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Result string `json:"result"`
		}
		if err := g.getCursorJSON(ctx, fmt.Sprintf("%s/agents/%s/runs/%s", baseURL, agentID, runID), key, &run); err != nil {
			return "", err
		}
		status := strings.ToLower(run.Status)
		switch status {
		case "finished", "success", "completed":
			if strings.TrimSpace(run.Result) == "" {
				return "", fmt.Errorf("cursor_agent: finished with empty result")
			}
			return run.Result, nil
		case "error", "failed", "cancelled", "canceled", "expired":
			return "", fmt.Errorf("cursor_agent: run ended with status %s: %s", run.Status, run.Result)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("cursor_agent: timed out waiting for run %s", runID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func addReasoning(body map[string]interface{}, level string) {
	switch strings.ToLower(level) {
	case "low":
		body["reasoning_effort"] = "low"
	case "medium", "med", "":
		body["reasoning_effort"] = "medium"
	case "high":
		body["reasoning_effort"] = "high"
	case "xhigh", "extra", "max":
		body["reasoning_effort"] = "high"
	}
}

func cursorMode(mode string) string {
	if mode == "plan" {
		return "plan"
	}
	return "agent"
}

func cursorParams(level string) []map[string]string {
	switch strings.ToLower(level) {
	case "low":
		return []map[string]string{{"id": "fast", "value": "true"}}
	case "high":
		return []map[string]string{{"id": "thinking", "value": "true"}}
	case "xhigh":
		return []map[string]string{{"id": "max", "value": "true"}}
	default:
		return nil
	}
}

func (g *GeneratorClient) postJSON(ctx context.Context, endpoint, apiKey string, body, out interface{}) error {
	return g.doJSON(ctx, http.MethodPost, endpoint, apiKey, false, body, out)
}

func (g *GeneratorClient) postCursorJSON(ctx context.Context, endpoint, apiKey string, body, out interface{}) error {
	return g.doJSON(ctx, http.MethodPost, endpoint, apiKey, true, body, out)
}

func (g *GeneratorClient) getCursorJSON(ctx context.Context, endpoint, apiKey string, out interface{}) error {
	return g.doJSON(ctx, http.MethodGet, endpoint, apiKey, true, nil, out)
}

func (g *GeneratorClient) doJSON(ctx context.Context, method, endpoint, apiKey string, basicAuth bool, body, out interface{}) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		if basicAuth {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(apiKey+":")))
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("llm http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("llm read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("llm http %d: %s", resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
