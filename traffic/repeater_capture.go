package traffic

import (
	"context"
	"sync"

	"github.com/google/uuid"
	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

// This private correlation header is always removed before forwarding, even if
// its value is not a registered ticket. It is never an authorization credential.
const RepeaterCaptureHeader = "X-Gondar-Internal-Repeater"

type RepeaterCaptureResult struct{ TrafficID, Note string }
type RepeaterCaptureTicket struct {
	owner  *Traffic
	host   string
	token  string
	mu     sync.Mutex
	flowID string
	closed bool
	once   sync.Once
	done   chan RepeaterCaptureResult
}

var repeaterCaptureTickets sync.Map // token -> ticket
var repeaterCaptureFlows sync.Map   // flow id -> ticket
var repeaterCaptureHostMu sync.Mutex
var repeaterCaptureHosts = map[repeaterCaptureHost]int{}

type repeaterCaptureHost struct {
	owner *Traffic
	host  string
}

func (t *Traffic) NewRepeaterCapture(host string) *RepeaterCaptureTicket {
	r := &RepeaterCaptureTicket{owner: t, host: hostOnly(host), token: uuid.NewString(), done: make(chan RepeaterCaptureResult, 1)}
	repeaterCaptureHostMu.Lock()
	repeaterCaptureHosts[repeaterCaptureHost{t, r.host}]++
	repeaterCaptureHostMu.Unlock()
	repeaterCaptureTickets.Store(r.token, r)
	return r
}
func (r *RepeaterCaptureTicket) Token() string { return r.token }
func (r *RepeaterCaptureTicket) Wait(ctx context.Context) RepeaterCaptureResult {
	select {
	case result := <-r.done:
		return result
	case <-ctx.Done():
		return RepeaterCaptureResult{Note: "未取得本次请求的精确录制ID：" + ctx.Err().Error()}
	}
}
func (r *RepeaterCaptureTicket) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	id := r.flowID
	r.mu.Unlock()
	repeaterCaptureTickets.Delete(r.token)
	if id != "" {
		repeaterCaptureFlows.Delete(id)
	}
	repeaterCaptureHostMu.Lock()
	key := repeaterCaptureHost{r.owner, r.host}
	if repeaterCaptureHosts[key] <= 1 {
		delete(repeaterCaptureHosts, key)
	} else {
		repeaterCaptureHosts[key]--
	}
	repeaterCaptureHostMu.Unlock()
}

// Keep this tunnel intercepted until the client has stopped using it. Another
// request may otherwise mark the host as passthrough between our initial check
// and CONNECT, allowing a private correlation header to reach the target.
func (t *Traffic) repeaterMustIntercept(host string) bool {
	repeaterCaptureHostMu.Lock()
	defer repeaterCaptureHostMu.Unlock()
	return repeaterCaptureHosts[repeaterCaptureHost{t, hostOnly(host)}] > 0
}
func (t *Traffic) RepeaterCaptureSupported(host string) bool {
	_, passthrough := t.pass.Load(hostOnly(host))
	return !passthrough
}

func (s *sink) Requestheaders(f *mproxy.Flow) {
	if f == nil || f.Request == nil {
		return
	}
	token := f.Request.Header.Get(RepeaterCaptureHeader)
	f.Request.Header.Del(RepeaterCaptureHeader)
	value, ok := repeaterCaptureTickets.Load(token)
	if !ok {
		return
	}
	r := value.(*RepeaterCaptureTicket)
	if r.owner != s.t {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.flowID != "" {
		return
	}
	r.flowID = f.Id.String()
	repeaterCaptureFlows.Store(r.flowID, r)
}

func (t *Traffic) completeRepeaterCapture(f *mproxy.Flow) {
	t.completeRepeaterCaptureWithID(f, "")
}

func (t *Traffic) completeRepeaterCaptureWithID(f *mproxy.Flow, recordedID string) {
	if f == nil {
		return
	}
	id := f.Id.String()
	value, ok := repeaterCaptureFlows.LoadAndDelete(id)
	if !ok {
		return
	}
	r := value.(*RepeaterCaptureTicket)
	if r.owner != t {
		return
	}
	result := RepeaterCaptureResult{Note: "本次流量未落盘（可能为流式响应、录制压力或记录错误）；不会猜测其他流量ID"}
	if recordedID != "" {
		result = RepeaterCaptureResult{TrafficID: recordedID}
	}
	r.once.Do(func() { r.done <- result })
}
