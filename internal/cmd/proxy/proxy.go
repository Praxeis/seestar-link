// Package proxy exposes device-local TCP ports as local listeners over the Kalay P2P tunnel.
//
// Ports are mapped Docker style, -p local:device, and may be given more than once. All
// mappings share one tunnel: the first is the tunnel's own channel and the rest are extra
// P2PTunnel channels within the same session, because the device rejects a second
// concurrent Kalay session.
//
// The listeners stay up for the life of the process. The tunnel is dialled once and held;
// when it drops, the proxy redials with backoff and reattaches every mapping, so a client
// only has to reconnect. A local status endpoint reports the link state as JSON.
//
// Useful device ports: 4700 JSON-RPC, 32323 ASCOM Alpaca REST, 80 HTTP image server.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Praxeis/seestar-link/kalay"
	"github.com/Praxeis/seestar-link/seestar"
)

// Mapping is one local:device port pair.
type Mapping struct{ Local, Device int }

func (m Mapping) String() string { return fmt.Sprintf("%d:%d", m.Local, m.Device) }

// ParseMapping parses "local:device", or a bare "port" meaning the same number on both sides.
func ParseMapping(v string) (Mapping, error) {
	l, d, ok := strings.Cut(v, ":")
	if !ok {
		d = l
	}
	lp, err := strconv.Atoi(strings.TrimSpace(l))
	if err != nil || lp < 1 || lp > 65535 {
		return Mapping{}, fmt.Errorf("bad local port in %q (want local:device)", v)
	}
	dp, err := strconv.Atoi(strings.TrimSpace(d))
	if err != nil || dp < 1 || dp > 65535 {
		return Mapping{}, fmt.Errorf("bad device port in %q (want local:device)", v)
	}
	return Mapping{Local: lp, Device: dp}, nil
}

// Options configures Run.
type Options struct {
	Profile    string    // profile name, for the status report only
	Listen     string    // bind address for the mapped ports
	Maps       []Mapping // at least one
	StatusAddr string    // status endpoint address; empty disables it
	Reconnect  bool      // redial when the tunnel drops instead of returning
	ClientWait time.Duration
	Logf       func(format string, args ...any)
}

// tunnel is what the proxy needs from a device session; *kalay.Tunnel satisfies it.
type tunnel interface {
	io.ReadWriteCloser
	OpenChannel(port int) (io.ReadWriteCloser, error)
	Done() <-chan struct{}
	Err() error
}

// dialTunnel opens a session to a device port. Tests replace it.
var dialTunnel = func(cfg seestar.ConnectConfig, port int) (tunnel, error) {
	t, err := seestar.Dial(cfg, port)
	if err != nil {
		return nil, err
	}
	return t, nil
}

var _ tunnel = (*kalay.Tunnel)(nil)

// Backoff between dial attempts. The first retry waits out the device's cooldown after a
// disconnect; later ones grow so a scope that is off or busy is not hammered.
var backoff = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}

// A session that stayed up this long resets the backoff to its first step.
const stableAfter = 2 * time.Minute

// Run binds the listeners, then holds a tunnel to the device until ctx is cancelled,
// redialling on loss when opts.Reconnect is set. It returns nil on cancellation.
func Run(ctx context.Context, cfg seestar.ConnectConfig, opts Options) error {
	if len(opts.Maps) == 0 {
		return errors.New("proxy: no port mappings")
	}
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1"
	}
	if opts.ClientWait == 0 {
		opts.ClientWait = 30 * time.Second
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	p := &proxy{opts: opts, cfg: cfg, state: "starting", since: time.Now(), up: make(chan struct{})}

	// Bind before dialling: a port clash then costs nothing, where dialling first would
	// burn the device's single session before discovering it.
	var lns []net.Listener
	closeAll := func() {
		for _, ln := range lns {
			ln.Close()
		}
	}
	for i, m := range opts.Maps {
		ln, err := net.Listen("tcp", net.JoinHostPort(opts.Listen, strconv.Itoa(m.Local)))
		if err != nil {
			closeAll()
			return fmt.Errorf("listen: %w", err)
		}
		lns = append(lns, ln)
		p.fwds = append(p.fwds, &forwarder{p: p, m: m, primary: i == 0})
	}
	defer closeAll()

	if opts.StatusAddr != "" {
		srv, err := p.serveStatus(opts.StatusAddr)
		if err != nil {
			closeAll()
			return err
		}
		defer srv.Close()
	}

	for i, f := range p.fwds {
		go f.accept(ctx, lns[i])
		opts.Logf("listening %s -> device :%d", lns[i].Addr(), f.m.Device)
	}

	return p.supervise(ctx)
}

type proxy struct {
	opts Options
	cfg  seestar.ConnectConfig
	fwds []*forwarder

	mu         sync.Mutex
	tun        tunnel
	up         chan struct{} // closed while connected; replaced on each drop
	state      string
	since      time.Time
	lastErr    string
	connects   int
	attempts   int
	nextTry    time.Time
	sessionBeg time.Time
}

// supervise dials, attaches the mappings, waits for the tunnel to end, and repeats.
func (p *proxy) supervise(ctx context.Context) error {
	step := 0
	for {
		p.setState("connecting", "")
		tun, streams, err := p.dial()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			p.opts.Logf("connect failed: %v", err)
			if !p.opts.Reconnect {
				return err
			}
			// A fresh login on the next attempt, in case the cached token went stale.
			p.cfg.Token = ""
			if !p.wait(ctx, backoff[min(step, len(backoff)-1)], err) {
				return nil
			}
			step++
			continue
		}

		began := time.Now()
		p.mu.Lock()
		p.tun = tun
		p.connects++
		p.sessionBeg = began
		p.mu.Unlock()
		for i, f := range p.fwds {
			f.attach(streams[i])
		}
		p.setState("connected", "")
		p.mu.Lock()
		close(p.up)
		p.mu.Unlock()
		p.opts.Logf("tunnel up to %s (%s)", p.cfg.DeviceSn, p.cfg.DeviceModel)

		var cause error
		select {
		case <-ctx.Done():
		case <-tun.Done():
			cause = tun.Err()
		}

		p.mu.Lock()
		p.tun = nil
		p.up = make(chan struct{})
		p.mu.Unlock()
		for _, f := range p.fwds {
			f.detach()
		}
		tun.Close()

		if ctx.Err() != nil {
			p.opts.Logf("released the device session")
			return nil
		}
		p.opts.Logf("tunnel lost after %s: %v", time.Since(began).Round(time.Second), cause)
		if !p.opts.Reconnect {
			return fmt.Errorf("tunnel lost: %w", cause)
		}
		if time.Since(began) >= stableAfter {
			step = 0
		}
		if !p.wait(ctx, backoff[min(step, len(backoff)-1)], cause) {
			return nil
		}
		step++
	}
}

// dial opens the tunnel on the first mapping and an extra channel for each other mapping.
func (p *proxy) dial() (tunnel, []io.ReadWriteCloser, error) {
	p.mu.Lock()
	p.attempts++
	p.mu.Unlock()
	tun, err := dialTunnel(p.cfg, p.opts.Maps[0].Device)
	if err != nil {
		return nil, nil, err
	}
	streams := []io.ReadWriteCloser{tun}
	for _, m := range p.opts.Maps[1:] {
		ch, err := tun.OpenChannel(m.Device)
		if err != nil {
			tun.Close()
			return nil, nil, fmt.Errorf("open channel to :%d: %w", m.Device, err)
		}
		streams = append(streams, ch)
	}
	return tun, streams, nil
}

// wait sleeps d before the next attempt, reporting it in the status. It returns false if
// ctx was cancelled meanwhile.
func (p *proxy) wait(ctx context.Context, d time.Duration, cause error) bool {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	p.setState("waiting", msg)
	p.mu.Lock()
	p.nextTry = time.Now().Add(d)
	p.mu.Unlock()
	p.opts.Logf("retrying in %s", d)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *proxy) setState(s, lastErr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != s {
		p.state, p.since = s, time.Now()
	}
	if lastErr != "" {
		p.lastErr = lastErr
	}
}

// upChan returns the channel that is closed while the tunnel is connected.
func (p *proxy) upChan() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.up
}

// forwarder carries one mapped port. While the tunnel is up it holds a stream to the device
// port. A reader runs for the stream's lifetime, discarding output when no client is
// attached: the device pushes unsolicited data, which would otherwise queue and be delivered
// to whoever connected next.
type forwarder struct {
	p       *proxy
	m       Mapping
	primary bool

	mu     sync.Mutex
	stream io.ReadWriteCloser
	client net.Conn

	clients  atomic.Int64
	toDevice atomic.Int64
	toClient atomic.Int64
}

var errBusy = errors.New("another client is connected")

func (f *forwarder) attach(s io.ReadWriteCloser) {
	f.mu.Lock()
	f.stream = s
	f.mu.Unlock()
	go f.readLoop(s)
}

// detach drops the stream and the client, if any. The client sees its connection close and
// can reconnect once the tunnel is back.
func (f *forwarder) detach() {
	f.mu.Lock()
	s, c := f.stream, f.client
	f.stream, f.client = nil, nil
	f.mu.Unlock()
	if s != nil && !f.primary {
		s.Close()
	}
	if c != nil {
		c.Close()
	}
}

func (f *forwarder) readLoop(s io.ReadWriteCloser) {
	buf := make([]byte, 64*1024)
	for {
		n, err := s.Read(buf)
		if n > 0 {
			f.mu.Lock()
			if f.client != nil && f.stream == s {
				if _, werr := f.client.Write(buf[:n]); werr == nil {
					f.toClient.Add(int64(n))
				}
			}
			f.mu.Unlock()
		}
		if err != nil {
			f.mu.Lock()
			if f.stream == s {
				f.stream = nil
				if f.client != nil {
					f.client.Close()
					f.client = nil
				}
			}
			f.mu.Unlock()
			return
		}
	}
}

func (f *forwarder) accept(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go f.serve(ctx, conn)
	}
}

// serve attaches one client. When the tunnel is down it waits up to ClientWait for it to
// come back, so a client that connects during a redial is held rather than refused.
func (f *forwarder) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	s, err := f.claim(ctx, conn)
	if err != nil {
		replyError(conn, f.m.Device, err)
		return
	}
	f.clients.Add(1)
	f.p.opts.Logf("client %s -> device :%d", conn.RemoteAddr(), f.m.Device)

	buf := make([]byte, 32*1024)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				break
			}
			f.toDevice.Add(int64(n))
		}
		if rerr != nil {
			break
		}
	}

	f.mu.Lock()
	if f.client == conn {
		f.client = nil
	}
	f.mu.Unlock()
}

// claim makes conn the forwarder's client and returns the stream it should write to.
func (f *forwarder) claim(ctx context.Context, conn net.Conn) (io.ReadWriteCloser, error) {
	deadline := time.NewTimer(f.p.opts.ClientWait)
	defer deadline.Stop()
	for {
		up := f.p.upChan()
		f.mu.Lock()
		if f.client != nil {
			f.mu.Unlock()
			return nil, errBusy
		}
		if s := f.stream; s != nil {
			f.client = conn
			f.mu.Unlock()
			return s, nil
		}
		f.mu.Unlock()

		select {
		case <-up:
			// Connected. The redial normally attached this mapping already; only if its
			// stream is still missing (the device closed that channel) is it reopened on
			// the live tunnel rather than waiting for a full redial.
			f.mu.Lock()
			missing := f.stream == nil
			f.mu.Unlock()
			if missing {
				if err := f.reopen(up); err != nil {
					return nil, err
				}
			}
		case <-deadline.C:
			return nil, fmt.Errorf("device not connected (%s); try again shortly", f.p.statusLine())
		case <-ctx.Done():
			return nil, errors.New("proxy shutting down")
		}
	}
}

// reopen opens a fresh channel for a non-primary mapping on the current tunnel.
func (f *forwarder) reopen(up chan struct{}) error {
	f.p.mu.Lock()
	tun := f.p.tun
	current := f.p.up == up
	f.p.mu.Unlock()
	if tun == nil || !current {
		return nil // dropped or redialled meanwhile; claim loops and looks again
	}
	if f.primary {
		// The tunnel's own channel cannot be reopened; losing it means the session is
		// ending. Give the supervisor a moment to notice, then let claim look again.
		select {
		case <-tun.Done():
		case <-time.After(2 * time.Second):
		}
		return nil
	}
	ch, err := tun.OpenChannel(f.m.Device)
	if err != nil {
		return fmt.Errorf("reopen channel to :%d: %w", f.m.Device, err)
	}
	f.mu.Lock()
	if f.stream != nil {
		f.mu.Unlock()
		ch.Close()
		return nil
	}
	f.mu.Unlock()
	f.attach(ch)
	return nil
}

// replyError reports a failure to the client, which would otherwise see only an empty
// close. Port 4700 speaks newline-delimited JSON-RPC, so the reply matches that; other
// ports get a plain-text line rather than a reply in a protocol they do not speak.
func replyError(w io.Writer, port int, err error) {
	if port != 4700 {
		fmt.Fprintf(w, "proxy: %v\r\n", err)
		return
	}
	type rpcErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	b, mErr := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Error   rpcErr `json:"error"`
		ID      any    `json:"id"`
	}{JSONRPC: "2.0", Error: rpcErr{Code: -32000, Message: "proxy: " + err.Error()}})
	if mErr != nil {
		return
	}
	w.Write(append(b, '\r', '\n'))
}

// Status is the JSON served on the status endpoint.
type Status struct {
	Profile    string          `json:"profile,omitempty"`
	Device     string          `json:"device"`
	Model      string          `json:"model"`
	State      string          `json:"state"` // starting, connecting, connected, waiting
	Since      time.Time       `json:"since"`
	Uptime     string          `json:"uptime,omitempty"` // of the current session, when connected
	Connects   int             `json:"connects"`
	Attempts   int             `json:"attempts"`
	LastError  string          `json:"last_error,omitempty"`
	NextTry    *time.Time      `json:"next_try,omitempty"`
	Mappings   []MappingStatus `json:"mappings"`
	ListenHost string          `json:"listen"`
}

// MappingStatus reports one mapped port.
type MappingStatus struct {
	Local     int   `json:"local"`
	Device    int   `json:"device"`
	Attached  bool  `json:"attached"`
	Client    bool  `json:"client_connected"`
	Clients   int64 `json:"clients_served"`
	BytesUp   int64 `json:"bytes_to_device"`
	BytesDown int64 `json:"bytes_to_client"`
}

func (p *proxy) snapshot() Status {
	p.mu.Lock()
	st := Status{
		Profile:    p.opts.Profile,
		Device:     p.cfg.DeviceSn,
		Model:      p.cfg.DeviceModel,
		State:      p.state,
		Since:      p.since,
		Connects:   p.connects,
		Attempts:   p.attempts,
		LastError:  p.lastErr,
		ListenHost: p.opts.Listen,
	}
	if p.state == "connected" {
		st.Uptime = time.Since(p.sessionBeg).Round(time.Second).String()
	}
	if p.state == "waiting" {
		t := p.nextTry
		st.NextTry = &t
	}
	p.mu.Unlock()
	for _, f := range p.fwds {
		f.mu.Lock()
		ms := MappingStatus{
			Local: f.m.Local, Device: f.m.Device,
			Attached: f.stream != nil, Client: f.client != nil,
		}
		f.mu.Unlock()
		ms.Clients, ms.BytesUp, ms.BytesDown = f.clients.Load(), f.toDevice.Load(), f.toClient.Load()
		st.Mappings = append(st.Mappings, ms)
	}
	return st
}

func (p *proxy) statusLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastErr != "" {
		return p.state + ": " + p.lastErr
	}
	return p.state
}

// serveStatus starts the status endpoint: GET / returns Status as JSON, GET /healthz
// returns 200 while connected and 503 otherwise, for monitoring tools.
func (p *proxy) serveStatus(addr string) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("status endpoint: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(p.snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		st := p.snapshot()
		if st.State != "connected" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintln(w, st.State)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	p.opts.Logf("status on http://%s/", ln.Addr())
	return srv, nil
}

// FetchStatus reads a running proxy's status endpoint.
func FetchStatus(addr string) (Status, error) {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/")
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return Status{}, fmt.Errorf("status endpoint %s: %w", addr, err)
	}
	return st, nil
}
