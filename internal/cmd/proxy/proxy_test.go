package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Praxeis/seestar-link/seestar"
)

// fakeTun is a device session whose ports echo each line back prefixed with the session
// number and port, so a test can tell which tunnel answered.
type fakeTun struct {
	io.ReadWriteCloser // the primary channel
	id                 int
	done               chan struct{}
	once               sync.Once
	mu                 sync.Mutex
	chans              []io.Closer
}

func echo(dev net.Conn, tag string) {
	r := bufio.NewReader(dev)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fmt.Fprintf(dev, "%s %s", tag, line)
	}
}

func newFakeTun(id, port int) *fakeTun {
	cli, dev := net.Pipe()
	go echo(dev, fmt.Sprintf("t%d:%d", id, port))
	return &fakeTun{ReadWriteCloser: cli, id: id, done: make(chan struct{}), chans: []io.Closer{dev}}
}

func (f *fakeTun) OpenChannel(port int) (io.ReadWriteCloser, error) {
	select {
	case <-f.done:
		return nil, errors.New("closed")
	default:
	}
	cli, dev := net.Pipe()
	go echo(dev, fmt.Sprintf("t%d:%d", f.id, port))
	f.mu.Lock()
	f.chans = append(f.chans, dev, cli)
	f.mu.Unlock()
	return cli, nil
}

func (f *fakeTun) Done() <-chan struct{} { return f.done }

func (f *fakeTun) Err() error {
	select {
	case <-f.done:
		return errors.New("fake: link lost")
	default:
		return nil
	}
}

// kill simulates the device going silent: the session ends and every channel breaks.
func (f *fakeTun) kill() {
	f.once.Do(func() {
		close(f.done)
		f.ReadWriteCloser.Close()
		f.mu.Lock()
		for _, c := range f.chans {
			c.Close()
		}
		f.mu.Unlock()
	})
}

func (f *fakeTun) Close() error { f.kill(); return nil }

type harness struct {
	t      *testing.T
	mu     sync.Mutex
	tuns   []*fakeTun
	dials  atomic.Int32
	failN  atomic.Int32 // fail this many dials before succeeding
	ports  []int
	status string
	cancel context.CancelFunc
	done   chan error
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func start(t *testing.T, devicePorts ...int) *harness {
	t.Helper()
	oldBackoff, oldDial := backoff, dialTunnel
	backoff = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond}
	h := &harness{t: t, done: make(chan error, 1)}
	dialTunnel = func(cfg seestar.ConnectConfig, port int) (tunnel, error) {
		n := int(h.dials.Add(1))
		if h.failN.Load() > 0 {
			h.failN.Add(-1)
			return nil, errors.New("fake: device busy")
		}
		ft := newFakeTun(n, port)
		h.mu.Lock()
		h.tuns = append(h.tuns, ft)
		h.mu.Unlock()
		return ft, nil
	}
	t.Cleanup(func() { backoff, dialTunnel = oldBackoff, oldDial })

	var maps []Mapping
	for _, dp := range devicePorts {
		lp := freePort(t)
		h.ports = append(h.ports, lp)
		maps = append(maps, Mapping{Local: lp, Device: dp})
	}
	h.status = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.done <- Run(ctx, seestar.ConnectConfig{DeviceSn: "TEST", DeviceModel: "Fake"}, Options{
			Maps: maps, StatusAddr: h.status, Reconnect: true, ClientWait: 2 * time.Second,
			Logf: func(f string, a ...any) { t.Logf(f, a...) },
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return h
}

func (h *harness) tun(i int) *fakeTun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tuns[i]
}

// roundTrip connects to mapped port i, sends a line and returns the reply.
func (h *harness) roundTrip(i int, msg string) (string, net.Conn) {
	h.t.Helper()
	var c net.Conn
	var err error
	for range 50 {
		if c, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", h.ports[i])); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		h.t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "%s\n", msg)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		h.t.Fatalf("read reply on port %d: %v", h.ports[i], err)
	}
	return strings.TrimSpace(line), c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 300 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestForwardsAllMappings(t *testing.T) {
	h := start(t, 4700, 32323)
	if got, c := h.roundTrip(0, "hello"); got != "t1:4700 hello" {
		t.Fatalf("primary: got %q", got)
	} else {
		c.Close()
	}
	if got, c := h.roundTrip(1, "alpaca"); got != "t1:32323 alpaca" {
		t.Fatalf("extra channel: got %q", got)
	} else {
		c.Close()
	}
}

func TestReconnectsAfterDrop(t *testing.T) {
	h := start(t, 4700, 32323)
	got, c := h.roundTrip(0, "one")
	if got != "t1:4700 one" {
		t.Fatalf("got %q", got)
	}

	h.tun(0).kill()
	// The attached client is disconnected so it knows to reconnect.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("client stayed connected across a dropped tunnel")
	}
	c.Close()

	waitFor(t, "second dial", func() bool { return h.dials.Load() >= 2 })
	if got, c := h.roundTrip(0, "two"); got != "t2:4700 two" {
		t.Fatalf("after reconnect, primary: got %q", got)
	} else {
		c.Close()
	}
	if got, c := h.roundTrip(1, "three"); got != "t2:32323 three" {
		t.Fatalf("after reconnect, extra channel: got %q", got)
	} else {
		c.Close()
	}
}

// waitDown waits until the proxy has noticed the drop, so the next client is not handed
// the stale stream of the dead session.
func (h *harness) waitDown() {
	h.t.Helper()
	waitFor(h.t, "drop noticed", func() bool {
		st, err := FetchStatus(h.status)
		return err == nil && st.State != "connected"
	})
}

func TestRetriesFailedDials(t *testing.T) {
	h := start(t, 4700)
	waitFor(t, "connected", func() bool { st, err := FetchStatus(h.status); return err == nil && st.State == "connected" })
	// The scope reports busy for the next three attempts, then accepts.
	h.failN.Store(3)
	h.tun(0).kill()
	h.waitDown()
	if got, c := h.roundTrip(0, "x"); got != "t5:4700 x" {
		t.Fatalf("got %q after 3 failed redials", got)
	} else {
		c.Close()
	}
	st, err := FetchStatus(h.status)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "connected" || st.Connects != 2 || st.Attempts != 5 || !strings.Contains(st.LastError, "busy") {
		t.Fatalf("status = %+v", st)
	}
}

func TestClientWaitsForRedial(t *testing.T) {
	h := start(t, 4700)
	_, c := h.roundTrip(0, "warm")
	c.Close()
	h.failN.Store(2) // stays down for a couple of backoff steps
	h.tun(0).kill()
	h.waitDown()
	// Connecting while down is held until the tunnel is back, not refused.
	if got, c := h.roundTrip(0, "held"); got != "t4:4700 held" {
		t.Fatalf("got %q", got)
	} else {
		c.Close()
	}
}

func TestSecondClientRefused(t *testing.T) {
	h := start(t, 4700)
	_, c1 := h.roundTrip(0, "first")
	defer c1.Close()
	c2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", h.ports[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(c2).ReadString('\n')
	if !strings.Contains(line, "another client is connected") || !strings.Contains(line, `"jsonrpc":"2.0"`) {
		t.Fatalf("second client got %q", line)
	}
}

func TestHealthz(t *testing.T) {
	h := start(t, 4700)
	waitFor(t, "connected", func() bool {
		st, err := FetchStatus(h.status)
		return err == nil && st.State == "connected"
	})
}

func TestParseMapping(t *testing.T) {
	for in, want := range map[string]Mapping{"4700:4700": {4700, 4700}, "8080:80": {8080, 80}, "32323": {32323, 32323}, " 1 : 2 ": {1, 2}} {
		got, err := ParseMapping(in)
		if err != nil || got != want {
			t.Errorf("ParseMapping(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x:1", "1:x", "0:1", "1:70000"} {
		if _, err := ParseMapping(bad); err == nil {
			t.Errorf("ParseMapping(%q) accepted", bad)
		}
	}
}
