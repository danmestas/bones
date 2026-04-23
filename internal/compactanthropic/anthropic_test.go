package compactanthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

func TestSummarize_SendsRequestAndReturnsText(t *testing.T) {
	t.Helper()
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s, want POST", r.Method)
		}
		if want := "/v1/messages"; r.URL.Path != want {
			t.Fatalf("path=%s, want %s", r.URL.Path, want)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Fatalf("x-api-key=%q", r.Header.Get("x-api-key"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"short summary"}]}`))
	}))
	defer server.Close()

	s := Summarizer{Config: Config{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Model:   "claude-3-5-haiku-latest",
	}}
	out, err := s.Summarize(context.Background(), coord.CompactInput{
		TaskID:       "agent-infra-sum1",
		Title:        "closed task",
		Files:        []string{"/tmp/a.go"},
		Context:      map[string]string{"kind": "bug"},
		CreatedAt:    time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
		ClosedAt:     time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC),
		ClosedBy:     "agent-a",
		ClosedReason: "done",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out != "short summary" {
		t.Fatalf("out=%q, want short summary", out)
	}
	if got["model"] != "claude-3-5-haiku-latest" {
		t.Fatalf("model=%v", got["model"])
	}
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages=%T %#v", got["messages"], got["messages"])
	}
}

func TestConfigValidate_RejectsMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "missing api key", cfg: Config{BaseURL: "https://api.anthropic.com", Model: "m"}},
		{name: "missing base url", cfg: Config{APIKey: "k", Model: "m"}},
		{name: "missing model", cfg: Config{APIKey: "k", BaseURL: "https://api.anthropic.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err == nil {
				t.Fatal("Validate: want error, got nil")
			}
		})
	}
}
