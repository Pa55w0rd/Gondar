package listener

import (
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Observe actual Read entry/results instead of assuming the accept/read loop
// has advanced after a sleep. No listener production hooks are needed.
type observedListener struct {
	net.Listener
	entered chan struct{}
	read    chan int
}

type observedConn struct {
	net.Conn
	owner *observedListener
	once  sync.Once
}

func (l *observedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &observedConn{Conn: c, owner: l}, nil
}

func (c *observedConn) Read(p []byte) (int, error) {
	c.once.Do(func() { c.owner.entered <- struct{}{} })
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.owner.read <- n
	}
	return n, err
}

func capacityListener(t *testing.T, sessions, connections int) (*Manager, *Listener, *observedListener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	observed := &observedListener{Listener: ln, entered: make(chan struct{}, 64), read: make(chan int, 128)}
	m := NewManager()
	l := newListener("capacity", ln.Addr().(*net.TCPAddr).Port, observed, sessions, connections)
	m.listeners[l.Name] = l
	go l.acceptLoop()
	t.Cleanup(func() { _, _ = m.Stop(l.Name) })
	return m, l, observed
}

func awaitReadEntry(t *testing.T, observed *observedListener) {
	t.Helper()
	select {
	case <-observed.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted connection never entered Read")
	}
}

func awaitCapturedBytes(t *testing.T, observed *observedListener, want int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for total := 0; total < want; {
		select {
		case n := <-observed.read:
			total += n
		case <-deadline.C:
			t.Fatalf("reader did not consume %d bytes", want)
		}
	}
}

func awaitReaders(t *testing.T, l *Listener) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for l.Stats().ActiveConnections != 0 {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("connection readers did not exit")
		}
	}
}

func dialCapacity(t *testing.T, observed *observedListener) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", observed.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestListenerCapacityRejectsNewConnectionWithoutEvictingActive(t *testing.T) {
	m, l, observed := capacityListener(t, 3, 1)
	first := dialCapacity(t, observed)
	awaitReadEntry(t, observed)
	second := dialCapacity(t, observed)
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("over-capacity connection was not closed")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("over-capacity connection stayed open: %v", err)
	}
	stats := l.Stats()
	if stats.ActiveConnections != 1 || stats.RetainedSessions != 1 || stats.RejectedConnections != 1 {
		t.Fatalf("unexpected capacity stats: %+v", stats)
	}
	if _, err := io.WriteString(first, "retained"); err != nil {
		t.Fatalf("established connection was evicted: %v", err)
	}
	awaitCapturedBytes(t, observed, len("retained"))
	final, err := m.Stop(l.Name)
	if err != nil || len(final) != 1 || final[0].Preview != "retained" || final[0].Open {
		t.Fatalf("stop did not drain established session: %+v, %v", final, err)
	}
	if l.Stats().ActiveConnections != 0 {
		t.Fatal("stop retained active sockets")
	}
	_ = first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := first.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("stopped reader socket did not close: %v", err)
	}
}

func TestListenerHistoryOnlyRetiresClosedSessions(t *testing.T) {
	m, l, observed := capacityListener(t, 3, 1)
	for _, text := range []string{"first", "second", "third", "fourth", "fifth"} {
		c := dialCapacity(t, observed)
		awaitReadEntry(t, observed)
		if _, err := io.WriteString(c, text); err != nil {
			t.Fatal(err)
		}
		awaitCapturedBytes(t, observed, len(text))
		_ = c.Close()
		// The accept loop is still live; only Stop may Wait on its worker group.
		awaitReaders(t, l)
	}
	final, err := m.Stop(l.Name)
	if err != nil || len(final) != 3 {
		t.Fatalf("history bound: %+v, %v", final, err)
	}
	for i, want := range []string{"fifth", "fourth", "third"} {
		if final[i].Preview != want || final[i].Open {
			t.Fatalf("history %d: %+v, want %q closed", i, final[i], want)
		}
	}
	if l.Stats().RejectedConnections != 0 {
		t.Fatal("closed history incorrectly prevented a new session")
	}
}

func TestListenerSnapshotConcurrentWithCaptureAndStop(t *testing.T) {
	m, l, observed := capacityListener(t, 4, 2)
	c := dialCapacity(t, observed)
	awaitReadEntry(t, observed)
	stopSnapshots, snapshotsDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(snapshotsDone)
		for {
			select {
			case <-stopSnapshots:
				return
			default:
				_ = l.Snapshot()
				_ = l.Stats()
				runtime.Gosched()
			}
		}
	}()
	t.Cleanup(func() { close(stopSnapshots); <-snapshotsDone })
	payload := strings.Repeat("capture", 16*1024)
	if n, err := io.WriteString(c, payload); err != nil || n != len(payload) {
		t.Fatalf("write=%d %v", n, err)
	}
	awaitCapturedBytes(t, observed, len(payload))
	final, err := m.Stop(l.Name)
	if err != nil || len(final) != 1 || final[0].Open || final[0].BytesIn != int64(len(payload)) {
		t.Fatalf("final session=%+v, err=%v", final, err)
	}
	if len(final[0].Preview) != MaxCapturePerSession || final[0].LastActive.IsZero() {
		t.Fatalf("capture preview/activity: %+v", final[0])
	}
}
