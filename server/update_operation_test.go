package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gondar/selfupdate"
)

func TestUpdateOperationAcceptedIdentityAndLostProgress(t *testing.T) {
	oldHub, oldCache, oldVersion, oldRepo, oldKey := updHub, relCache, BuildVersion, selfupdate.Repo, selfupdate.ReleasePublicKey
	t.Cleanup(func() {
		updHub, relCache, BuildVersion, selfupdate.Repo, selfupdate.ReleasePublicKey = oldHub, oldCache, oldVersion, oldRepo, oldKey
	})
	BuildVersion, selfupdate.Repo = "1.0.0", "fixture/gondar"
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	selfupdate.ReleasePublicKey = base64.StdEncoding.EncodeToString(pub)
	var mu sync.Mutex
	var saved selfupdate.Operation
	var calls atomic.Int32
	finished, release := make(chan struct{}), make(chan struct{})
	h := &updateHub{subs: map[chan updateProgress]struct{}{}}
	h.persist = func(op selfupdate.Operation) error { mu.Lock(); defer mu.Unlock(); saved = op; return nil }
	h.load = func() (selfupdate.Operation, bool) { mu.Lock(); defer mu.Unlock(); return saved, saved.ID != "" }
	h.runtime = func() updateRuntime {
		return updateRuntime{Version: "1.0.0", BootID: "old-boot", BinarySHA256: "old-digest"}
	}
	h.stage = func(ctx context.Context, _ *http.Client, rel *selfupdate.Release, current string, op selfupdate.Operation, emit selfupdate.Progress) error {
		calls.Add(1)
		if op.TargetVersion != "1.1.0" || current != "1.0.0" || rel.TagName != "v1.1.0" || op.AcceptedBootID != "old-boot" {
			t.Error("accepted update target identity changed")
		}
		<-release
		if ctx.Err() != nil {
			t.Error("HTTP caller cancellation canceled installation")
		}
		op.Phase, op.BinarySHA256 = "staged", "verified-digest"
		return h.persist(op)
	}
	h.restart = func() { close(finished) }
	updHub = h
	relCache = &releaseCache{fetch: func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return &selfupdate.Release{TagName: "v1.1.0"}, nil
	}}
	s := &Server{ctx: context.Background()}
	post := func(body string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		ctx, cancel := context.WithCancel(context.Background())
		s.updateApply(out, httptest.NewRequest(http.MethodPost, "/api/update/apply", strings.NewReader(body)).WithContext(ctx))
		cancel() // the caller is gone, but an accepted installation still runs
		return out
	}
	// Do not subscribe to SSE at all: lost staged/progress must not hide receipt.
	body := `{"request_id":"fixture-request","target_version":"v1.1.0"}`
	ack := post(body)
	if ack.Code != 202 {
		t.Fatalf("apply status=%d body=%s", ack.Code, ack.Body.String())
	}
	var first struct {
		OperationID string `json:"operation_id"`
		Target      string `json:"target"`
	}
	if json.Unmarshal(ack.Body.Bytes(), &first) != nil || first.OperationID == "" || first.Target != "1.1.0" {
		t.Fatal("acceptance did not bind an exact target")
	}
	second := post(body)
	if second.Code != 202 || !strings.Contains(second.Body.String(), first.OperationID) {
		t.Fatal("same request was not idempotent")
	}
	if changed := post(`{"request_id":"fixture-request","target_version":"v1.2.0"}`); changed.Code != 409 {
		t.Fatal("request ID could change target")
	}
	if changed := post(`{"request_id":"different-request","target_version":"v1.2.0"}`); changed.Code != 409 {
		t.Fatal("stale target was accepted")
	}
	status := httptest.NewRecorder()
	s.updateStatus(status, httptest.NewRequest(http.MethodGet, "/api/update/status?request_id=fixture-request", nil))
	if status.Code != 200 || !strings.Contains(status.Body.String(), first.OperationID) {
		t.Fatal("request ID could not reconcile a lost acknowledgement")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic installation did not finish")
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate installations: %d", calls.Load())
	}
	// A new process has no in-memory hub, but reads the same durable receipt.
	updHub = &updateHub{subs: map[chan updateProgress]struct{}{}, load: h.load, runtime: func() updateRuntime {
		return updateRuntime{Version: "1.1.0", BootID: "new-boot", BinarySHA256: "verified-digest"}
	}}
	op, _ := h.load()
	op.Phase = "completed"
	_ = h.persist(op)
	status = httptest.NewRecorder()
	s.updateStatus(status, httptest.NewRequest(http.MethodGet, "/api/update/status?operation_id="+first.OperationID, nil))
	var observed struct {
		Operation selfupdate.Operation `json:"operation"`
		Runtime   updateRuntime        `json:"runtime"`
	}
	if json.Unmarshal(status.Body.Bytes(), &observed) != nil || observed.Operation.ID != first.OperationID || observed.Operation.Phase != "completed" || observed.Runtime.BootID == observed.Operation.AcceptedBootID || observed.Runtime.Version != observed.Operation.TargetVersion || observed.Runtime.BinarySHA256 != observed.Operation.BinarySHA256 {
		t.Fatal("post-restart status lost exact verified activation identity")
	}
}

func TestUpdateStatusRemainsAuthenticatedAndDoesNotExposeHealthIdentity(t *testing.T) {
	s := &Server{}
	called := false
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; s.updateStatus(w, r) }))
	for _, path := range []string{"/api/update/status", "/api/update/status?token=not-allowed-on-status"} {
		out := httptest.NewRecorder()
		h.ServeHTTP(out, httptest.NewRequest(http.MethodGet, path, nil))
		if out.Code != 401 || called || strings.Contains(out.Body.String(), "binary_sha256") {
			t.Fatal("unauthenticated update identity was exposed")
		}
	}
}

func TestUpdateOperationRequiresDurableAcceptanceAndStablePreviousBoot(t *testing.T) {
	h := &updateHub{subs: map[chan updateProgress]struct{}{}, load: func() (selfupdate.Operation, bool) { return selfupdate.Operation{}, false }, runtime: func() updateRuntime { return updateRuntime{Version: "1.0.0", BootID: "fixture-boot"} }, persist: func(selfupdate.Operation) error { return errors.New("fixture storage full") }}
	if _, start, err := h.beginOperation(updateApplyRequest{RequestID: "fixture-write-fails", TargetVersion: "v1.1.0"}, "v1.1.0"); !errors.Is(err, errUpdateAcceptanceStorage) || start || h.running || h.operation != nil {
		t.Fatal("installation could start without a durable receipt")
	}
	for _, phase := range []string{"activating", "settling"} {
		h.load = func() (selfupdate.Operation, bool) {
			return selfupdate.Operation{ID: "old", RequestID: "old-request", Phase: phase}, true
		}
		h.persist = func(selfupdate.Operation) error {
			t.Error("new acceptance overwrote an unconfirmed activation")
			return nil
		}
		if _, start, err := h.beginOperation(updateApplyRequest{RequestID: "another-update"}, "v1.2.0"); err == nil || start {
			t.Fatalf("new update accepted during %s", phase)
		}
	}
}
