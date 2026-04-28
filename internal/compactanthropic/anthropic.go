package compactanthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danmestas/bones/internal/coord"
)

const defaultAnthropicVersion = "2023-06-01"

type Config struct {
	APIKey  string
	BaseURL string
	Model   string
}

func (c Config) Validate() error {
	if c.APIKey == "" {
		return fmt.Errorf("compactanthropic.Config: APIKey: must be non-empty")
	}
	if c.BaseURL == "" {
		return fmt.Errorf("compactanthropic.Config: BaseURL: must be non-empty")
	}
	if c.Model == "" {
		return fmt.Errorf("compactanthropic.Config: Model: must be non-empty")
	}
	return nil
}

type Summarizer struct {
	Config     Config
	HTTPClient *http.Client
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicResponse struct {
	Content []anthropicContentBlock `json:"content"`
}

func (s Summarizer) Summarize(
	ctx context.Context,
	in coord.CompactInput,
) (string, error) {
	if err := s.Config.Validate(); err != nil {
		return "", err
	}
	body, err := buildRequestBody(s.Config.Model, in)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(s.Config.BaseURL, "/")+"/v1/messages",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("compactanthropic.Summarize: new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", s.Config.APIKey)
	req.Header.Set("anthropic-version", defaultAnthropicVersion)
	client := s.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("compactanthropic.Summarize: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return "", fmt.Errorf(
			"compactanthropic.Summarize: status %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(data)),
		)
	}
	var decoded anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("compactanthropic.Summarize: decode response: %w", err)
	}
	for _, block := range decoded.Content {
		if block.Type == "text" && block.Text != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("compactanthropic.Summarize: no text content in response")
}

func buildRequestBody(model string, in coord.CompactInput) ([]byte, error) {
	inputJSON, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("compactanthropic.Summarize: marshal input: %w", err)
	}
	systemPrompt := "Summarize a closed engineering task for long-horizon " +
		"agent memory. Return only a concise markdown summary with the " +
		"important outcome, relevant files, and any follow-up context."
	payload := anthropicRequest{
		Model:     model,
		MaxTokens: 400,
		System:    systemPrompt,
		Messages: []anthropicMessage{{
			Role: "user",
			Content: []anthropicContentBlock{{
				Type: "text",
				Text: "Summarize this closed task for archive compaction:\n\n" + string(inputJSON),
			}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("compactanthropic.Summarize: marshal request: %w", err)
	}
	return body, nil
}
