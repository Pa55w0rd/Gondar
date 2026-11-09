package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

type gateRoundTripper func(*http.Request) (*http.Response, error)

func (f gateRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errorGateBody struct{}

func (errorGateBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (errorGateBody) Close() error             { return nil }

func awaitRequestGate(t *testing.T, key [32]byte, refs, active int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		modelRequestGates.mu.Lock()
		g := modelRequestGates.entries[key]
		gotRefs, gotActive := -1, -1
		if g != nil {
			gotRefs = g.refs
			g.mu.Lock()
			gotActive = g.active
			g.mu.Unlock()
		}
		modelRequestGates.mu.Unlock()
		if gotRefs == refs && gotActive == active {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("gate did not reach refs=%d active=%d", refs, active)
}

func TestModelRequestGateSharedIdentityReleasesBodies(t *testing.T) {
	for _, release := range []string{"eof", "close", "read_error", "cancel"} {
		t.Run(release, func(t *testing.T) {
			var calls atomic.Int64
			base := gateRoundTripper(func(*http.Request) (*http.Response, error) {
				var body io.ReadCloser = io.NopCloser(strings.NewReader("body"))
				if release == "read_error" && calls.Load() == 0 {
					body = errorGateBody{}
				}
				calls.Add(1)
				return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
			})
			cfg := Config{BaseURL: "https://gate.invalid/" + t.Name(), APIKey: "synthetic-key", MaxConcurrent: 1, LimitOwnerID: 81001}
			a, err := limitModelRequests(base, cfg)
			if err != nil {
				t.Fatal(err)
			}
			other := cfg
			other.MaxConcurrent = 0
			other.LimitOwnerID = 81002
			other.Model = "different-model"
			b, err := limitModelRequests(base, other)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", cfg.BaseURL, nil)
			first, err := a.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Body.Close()
			done := make(chan *http.Response, 1)
			fail := make(chan error, 1)
			go func() {
				r, e := b.RoundTrip(req.Clone(t.Context()))
				if e != nil {
					fail <- e
				} else {
					done <- r
				}
			}()
			awaitRequestGate(t, requestIdentity(cfg), 2, 1)
			if calls.Load() != 1 {
				t.Fatal("duplicate profile bypassed the cap")
			}
			switch release {
			case "eof":
				_, _ = io.ReadAll(first.Body)
			case "close":
				_ = first.Body.Close()
			case "read_error":
				_, _ = first.Body.Read(make([]byte, 4))
			case "cancel":
				cancel()
			}
			select {
			case second := <-done:
				_ = second.Body.Close()
			case err := <-fail:
				t.Fatal(err)
			case <-time.After(3 * time.Second):
				t.Fatal("slot leaked")
			}
			_ = first.Body.Close() // repeated close must not decrement a second time
			awaitRequestGate(t, requestIdentity(cfg), 0, 0)
		})
	}
}

func TestModelRequestGateWaitingCancellationAndTransportError(t *testing.T) {
	var calls atomic.Int64
	base := gateRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("x"))}, nil
		}
		return nil, errors.New("fixture transport failed")
	})
	cfg := Config{BaseURL: "https://wait.invalid/v1", APIKey: "wait-key", MaxConcurrent: 1, LimitOwnerID: 82001}
	rt, err := limitModelRequests(base, cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", cfg.BaseURL, nil)
	first, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, e := rt.RoundTrip(req.Clone(ctx)); done <- e }()
	awaitRequestGate(t, requestIdentity(cfg), 2, 1)
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatalf("wait error=%v", e)
	}
	if calls.Load() != 1 {
		t.Fatal("canceled waiter reached transport")
	}
	_ = first.Body.Close()
	if _, e := rt.RoundTrip(req); e == nil {
		t.Fatal("fixture error lost")
	}
	awaitRequestGate(t, requestIdentity(cfg), 0, 0)
}

func TestModelRequestAttemptRateCoversSDKRetry(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		n := len(starts)
		mu.Unlock()
		if n == 1 {
			http.Error(w, "temporary", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	cfg := Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "retry-key", Model: "fixture", RatePerSecond: 40, RatePerMinute: 1200,
		Retry: RetryConfig{ConnectAttempts: 2, ConnectInterval: time.Nanosecond}}
	p, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = p.Complete(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}, MaxTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 {
		t.Fatalf("actual attempts=%d", len(starts))
	}
	if d := starts[1].Sub(starts[0]); d < 40*time.Millisecond {
		t.Fatalf("HTTP retry bypassed 1200RPM spacing: %v", d)
	}
}

func TestModelRequestRegistryBoundedAndHotReload(t *testing.T) {
	r := requestGateRegistry{entries: make(map[[32]byte]*requestGate), capacity: 2}
	key := sha256.Sum256([]byte("first"))
	g, err := r.gate(key, "profile:1", requestLimits{concurrent: 1}, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.gate(key, "profile:1", requestLimits{concurrent: 3}, true)
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	cap := g.owners["profile:1"].concurrent
	g.mu.Unlock()
	if cap != 3 {
		t.Fatal("hot reload accumulated stale owner limits")
	}
	for i := 0; i < 20; i++ {
		k := sha256.Sum256([]byte{byte(i)})
		if _, e := r.gate(k, "fixture", requestLimits{}, true); e != nil {
			t.Fatal(e)
		}
	}
	if len(r.entries) != 2 {
		t.Fatal("registry grew beyond bound")
	}
	// In-flight entries cannot be evicted and split into independent lanes.
	for _, e := range r.entries {
		e.refs = 1
	}
	if _, err = r.gate(key, "fixture", requestLimits{}, false); err == nil {
		t.Fatal("evicted active identities")
	}
	a := Config{BaseURL: "https://API.EXAMPLE:443/v1/", APIKey: "k"}
	b := a
	b.BaseURL = "https://api.example/v1"
	b.Model = "other"
	if requestIdentity(a) != requestIdentity(b) {
		t.Fatal("equivalent API origins got separate gates")
	}
}
