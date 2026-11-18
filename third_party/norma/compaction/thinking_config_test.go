package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// Use actual adapters and loopback responses: an empty request override must
// preserve both explicit operator choices and the default of omitting the field.
func TestProviderSummarizerInheritsThinkingConfigOnWire(t *testing.T) {
	for _, format := range []llm.Format{llm.FormatAnthropic, llm.FormatOpenAI} {
		for _, thinking := range []string{"", "enabled", "disabled", "adaptive"} {
			for _, nonStreaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/nonStreaming=%v", format, thinking, nonStreaming), func(t *testing.T) {
					captured := make(chan map[string]any, 1)
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							http.Error(w, "invalid synthetic request", http.StatusBadRequest)
							return
						}
						captured <- body
						if nonStreaming {
							w.Header().Set("Content-Type", "application/json")
							if format == llm.FormatAnthropic {
								fmt.Fprint(w, `{"content":[{"type":"text","text":"<summary>fixture</summary>"}],"stop_reason":"end_turn"}`)
							} else {
								fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"<summary>fixture</summary>"},"finish_reason":"stop"}]}`)
							}
						} else {
							w.Header().Set("Content-Type", "text/event-stream")
							if format == llm.FormatAnthropic {
								fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"<summary>fixture</summary>\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
							} else {
								fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"<summary>fixture</summary>\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
							}
						}
					}))
					defer srv.Close()
					p, err := llm.NewProvider(llm.Config{Format: format, BaseURL: srv.URL, Model: "fixture", HTTPClient: srv.Client(), ThinkingType: thinking, MaxRetries: -1, EmptyResponseRetries: -1})
					if err != nil {
						t.Fatal(err)
					}
					got, err := ProviderSummarizer(p, 1234, nonStreaming)(t.Context(), []llm.Message{llm.UserText("saved synthetic observation")})
					if err != nil || got != "Summary:\nfixture" {
						t.Fatalf("summary=%q err=%v", got, err)
					}
					body := <-captured
					if thinking == "" {
						if _, exists := body["thinking"]; exists {
							t.Fatalf("default must omit thinking: %+v", body["thinking"])
						}
					} else if field, _ := body["thinking"].(map[string]any); field["type"] != thinking {
						t.Fatalf("profile thinking=%q overridden on wire: %+v", thinking, body["thinking"])
					}
					if body["max_tokens"] != float64(1234) {
						t.Fatalf("output cap changed: %v", body["max_tokens"])
					}
					if tools, exists := body["tools"]; exists {
						t.Fatalf("summary gained tools: %v", tools)
					}
				})
			}
		}
	}
}

func TestProviderSummarizerCancellationInterruptsLiveRequest(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		t.Run(fmt.Sprint(nonStreaming), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer func() { close(release); srv.Close() }()
			p, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, Model: "fixture", HTTPClient: srv.Client(), MaxRetries: -1, EmptyResponseRetries: -1})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := ProviderSummarizer(p, 1234, nonStreaming)(ctx, []llm.Message{llm.UserText("saved observation")})
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach loopback fixture")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled summary did not return")
			}
		})
	}
}
