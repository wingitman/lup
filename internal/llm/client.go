// Package llm provides a lightweight OpenAI-compatible HTTP client for
// chat completions and embeddings.  It works with any server that speaks the
// OpenAI REST API — Ollama, LM Studio, vLLM, the real OpenAI, etc.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is an OpenAI-compatible HTTP client.
type Client struct {
	baseURL    string
	apiKey     string
	chatModel  string
	embedModel string
	http       *http.Client
}

// New creates a Client.
func New(baseURL, apiKey, chatModel, embedModel string, timeoutSecs int) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		chatModel:  chatModel,
		embedModel: embedModel,
		http:       &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second},
	}
}

// ──────────────────────────────────────────────────────────
// Chat completions
// ──────────────────────────────────────────────────────────

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float32       `json:"temperature"`
}

type chatChoice struct {
	Message chatMessage `json:"message"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Error   *apiError    `json:"error,omitempty"`
}

// Complete sends a single user prompt and returns the assistant reply.
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	req := chatRequest{
		Model:       c.chatModel,
		Temperature: 0.2,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	var resp chatResponse
	if err := c.post(ctx, "/chat/completions", req, &resp); err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", fmt.Errorf("llm: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("llm: empty response from model")
	}
	return resp.Choices[0].Message.Content, nil
}

// ──────────────────────────────────────────────────────────
// Embeddings
// ──────────────────────────────────────────────────────────

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedData struct {
	Embedding []float32 `json:"embedding"`
}

type embedResponse struct {
	Data  []embedData `json:"data"`
	Error *apiError   `json:"error,omitempty"`
}

// Embed returns a vector embedding for the given text.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	req := embedRequest{
		Model: c.embedModel,
		Input: text,
	}

	var resp embedResponse
	if err := c.post(ctx, "/embeddings", req, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("llm embed: %s", resp.Error.Message)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("llm embed: empty response from model")
	}
	return resp.Data[0].Embedding, nil
}

// ──────────────────────────────────────────────────────────
// Shared HTTP helper
// ──────────────────────────────────────────────────────────

type apiError struct {
	Message string `json:"message"`
}

func (c *Client) post(ctx context.Context, path string, body, out interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
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

	return json.Unmarshal(raw, out)
}
