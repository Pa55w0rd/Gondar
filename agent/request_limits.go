package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type requestLimits struct {
	concurrent int
	second     float64
	minute     float64
}

type requestGate struct {
	mu      sync.Mutex
	owners  map[string]requestLimits
	active  int
	last    time.Time
	changed chan struct{}
	refs    int // under registry.mu: waiting attempts and bodies not yet released
	used    uint64
}

type requestGateRegistry struct {
	mu       sync.Mutex
	entries  map[[32]byte]*requestGate
	clock    uint64
	capacity int
}

var modelRequestGates = requestGateRegistry{entries: make(map[[32]byte]*requestGate), capacity: 256}

func requestIdentity(c Config) [32]byte {
	base := c.BaseURL
	if base == "" {
		switch c.Provider() {
		case "openai", "openai-responses":
			base = "https://api.openai.com/v1"
		default:
			base = "https://api.anthropic.com"
		}
	}
	if u, err := url.Parse(base); err == nil {
		u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
		if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
			host := u.Hostname()
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			u.Host = host
		}
		u.Fragment, u.RawQuery = "", ""
		base = strings.TrimRight(u.String(), "/")
	}
	// Models and profile names are intentionally absent: duplicate profiles
	// using the same credential and API base must not create extra quota lanes.
	return sha256.Sum256([]byte(base + "\x00" + c.APIKey))
}

func requestOwner(c Config) string {
	if c.LimitOwnerID > 0 {
		return fmt.Sprintf("profile:%d", c.LimitOwnerID)
	}
	return fmt.Sprintf("config:%g:%g:%d", c.RatePerSecond, c.RatePerMinute, c.MaxConcurrent)
}

func (r *requestGateRegistry) gate(key [32]byte, owner string, limits requestLimits, update bool) (*requestGate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.entries[key]
	if g == nil {
		if len(r.entries) >= r.capacity {
			var oldest [32]byte
			var victim *requestGate
			for id, candidate := range r.entries {
				if candidate.refs == 0 && (victim == nil || candidate.used < victim.used) {
					oldest, victim = id, candidate
				}
			}
			if victim == nil {
				return nil, errors.New("模型请求治理身份容量已满，请稍后重试")
			}
			delete(r.entries, oldest)
		}
		g = &requestGate{owners: make(map[string]requestLimits), changed: make(chan struct{})}
		r.entries[key] = g
	}
	r.clock++
	g.used = r.clock
	g.mu.Lock()
	_, known := g.owners[owner]
	if !known && len(g.owners) >= 256 {
		g.mu.Unlock()
		return nil, errors.New("同一模型身份的配置数量超过上限")
	}
	if update || !known {
		g.owners[owner] = limits
		close(g.changed)
		g.changed = make(chan struct{})
	}
	g.mu.Unlock()
	if !update {
		g.refs++
	}
	return g, nil
}

// ForgetRequestLimitOwner removes a deleted profile's policy. Limits of other
// profiles sharing the same identity remain in force.
func ForgetRequestLimitOwner(profileID int64) {
	owner := fmt.Sprintf("profile:%d", profileID)
	modelRequestGates.mu.Lock()
	defer modelRequestGates.mu.Unlock()
	for _, g := range modelRequestGates.entries {
		g.mu.Lock()
		delete(g.owners, owner)
		close(g.changed)
		g.changed = make(chan struct{})
		g.mu.Unlock()
	}
}

func (g *requestGate) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.mu.Lock()
		maximum := 0
		var spacing time.Duration
		for _, limits := range g.owners {
			if limits.concurrent > 0 && (maximum == 0 || limits.concurrent < maximum) {
				maximum = limits.concurrent
			}
			for _, candidate := range []struct {
				count  float64
				window time.Duration
			}{{limits.second, time.Second}, {limits.minute, time.Minute}} {
				if candidate.count > 0 {
					d := time.Duration(float64(candidate.window) / candidate.count)
					if d < time.Nanosecond {
						d = time.Nanosecond
					}
					if d > spacing {
						spacing = d
					}
				}
			}
		}
		delay := time.Until(g.last.Add(spacing))
		changed := g.changed
		if maximum == 0 || g.active < maximum {
			if delay <= 0 {
				g.active++
				g.last = time.Now()
				g.mu.Unlock()
				return nil
			}
		} else {
			delay = 0
		}
		g.mu.Unlock()
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-changed:
				timer.Stop()
			case <-timer.C:
			}
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
			}
		}
	}
}

type requestLimitedTransport struct {
	base     http.RoundTripper
	key      [32]byte
	owner    string
	limits   requestLimits
	registry *requestGateRegistry
}

func limitModelRequests(base http.RoundTripper, c Config) (http.RoundTripper, error) {
	if c.MaxConcurrent < 0 || c.MaxConcurrent > 1024 {
		return nil, errors.New("max_concurrent 必须为0–1024，0表示不限")
	}
	t := &requestLimitedTransport{base: base, key: requestIdentity(c), owner: requestOwner(c),
		limits: requestLimits{concurrent: c.MaxConcurrent, second: c.RatePerSecond, minute: c.RatePerMinute}, registry: &modelRequestGates}
	if _, err := t.registry.gate(t.key, t.owner, t.limits, true); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *requestLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g, err := t.registry.gate(t.key, t.owner, t.limits, false)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	releaseRef := func() { t.registry.mu.Lock(); g.refs--; t.registry.mu.Unlock() }
	if err := g.wait(req.Context()); err != nil {
		releaseRef()
		return nil, err
	}
	releaseSlot := func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			close(g.changed)
			g.changed = make(chan struct{})
			g.mu.Unlock()
			releaseRef()
		})
	}
	stop := context.AfterFunc(req.Context(), releaseSlot)
	release := func() { stop(); releaseSlot() }
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		release()
		return resp, err
	}
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody {
		release()
		return resp, nil
	}
	resp.Body = &requestSlotBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

type requestSlotBody struct {
	io.ReadCloser
	release func()
}

func (b *requestSlotBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}
func (b *requestSlotBody) Close() error { defer b.release(); return b.ReadCloser.Close() }
