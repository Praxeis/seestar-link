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
	// The first mapping rides the tunnel's own channel, which cannot be reopened, so it must
	// be a port that keeps its socket open. Move a port 80 mapping out of first place.
	maps := append([]Mapping(nil), opts.Maps...)
	for i, m := range maps {
		if m.Device != 80 {
			maps[0], maps[i] = maps[i], maps[0]
			break
		}
	}
	if maps[0].Device == 80 {
		opts.Logf("note: port 80 alone rides the tunnel's main channel, which the image server closes after one response; add -p 4700:4700 as well")
	}
	opts.Maps = maps
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
		p.fwds = append(p.fwds, &forwarder{p: p, m: m, primary: i == 0, perClient: i != 0 && m.Device == 80})
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

	openMu sync.Mutex // serialises channel opens

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
			if streams[i] != nil {
				f.attach(streams[i])
			}
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

// dial opens the tunnel on the first mapping and an extra channel for each other persistent
// mapping.
func (p *proxy) dial() (tunnel, []io.ReadWriteCloser, error) {
	p.mu.Lock()
	p.attempts++
	p.mu.Unlock()
	tun, err := dialTunnel(p.cfg, p.opts.Maps[0].Device)
	if err != nil {
		return nil, nil, err
	}
	streams := []io.ReadWriteCloser{tun}
	for _, f := range p.fwds[1:] {
		if f.perClient {
			streams = append(streams, nil) // opened per client
			continue
		}
		m := f.m
		ch, err := p.openChannel(tun, m.Device)
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

// forwarder carries one mapped port. While the tunnel is up a persistent forwarder holds a
// stream to the device port. A reader runs for the stream's lifetime, discarding output when
// no client is attached: the device pushes unsolicited data, which would otherwise queue and
// be delivered to whoever connected next.
//
// A per-client forwarder (device port 80) instead opens a fresh channel for every client and
// closes it when the client leaves, because the scope's image server closes its socket after
// each response and a reused channel would be dead for the next request. Several clients may
// use a per-client mapping at once, as browsers expect.
type forwarder struct {
	p         *proxy
	m         Mapping
	primary   bool
	perClient bool

	mu     sync.Mutex
	stream io.ReadWriteCloser
	client net.Conn
	active int // per-client connections in flight

	clients  atomic.Int64
	toDevice atomic.Int64
	toClient atomic.Int64
}

var errBusy = errors.New("another client is connected")

// clientWriteTimeout bounds a write to a client. One that stops reading (a sleeping laptop,
// a suspended app) is dropped rather than allowed to stall the device stream.
var clientWriteTimeout = 10 * time.Second

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

// dropClient disconnects c if it is still the attached client.
func (f *forwarder) dropClient(c net.Conn) {
	f.mu.Lock()
	if f.client == c {
		f.client = nil
	}
	f.mu.Unlock()
	c.Close()
}

func (f *forwarder) readLoop(s io.ReadWriteCloser) {
	buf := make([]byte, 64*1024)
	for {
		n, err := s.Read(buf)
		if n > 0 {
			// Write outside the lock, so a slow client cannot block detach, claim or the
			// status page. This loop is the only writer to the client, so order is kept.
			f.mu.Lock()
			c := f.client
			mine := f.stream == s
			f.mu.Unlock()
			if c != nil && mine {
				c.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
				if _, werr := c.Write(buf[:n]); werr != nil {
					f.dropClient(c)
				} else {
					f.toClient.Add(int64(n))
				}
			}
		}
		if err != nil {
			f.mu.Lock()
			var c net.Conn
			if f.stream == s {
				f.stream = nil
				c, f.client = f.client, nil
			}
			f.mu.Unlock()
			if c != nil {
				c.Close()
			}
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
		if f.perClient {
			go f.servePerClient(ctx, conn)
		} else {
			go f.serve(ctx, conn)
		}
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

// servePerClient gives one client its own channel for the length of its connection.
func (f *forwarder) servePerClient(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	ch, err := f.p.openFresh(ctx, f.m.Device)
	if err != nil {
		replyError(conn, f.m.Device, err)
		return
	}
	defer ch.Close()
	f.clients.Add(1)
	f.mu.Lock()
	f.active++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	go func() {
		io.Copy(countingWriter{conn, &f.toClient}, ch)
		conn.Close() // the device finished (or the tunnel dropped); end the client too
	}()
	io.Copy(countingWriter{ch, &f.toDevice}, conn)
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
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
			// stream is still missing is it reopened on the live tunnel.
			if err := f.reopen(up); err != nil {
				return nil, err
			}
		case <-deadline.C:
			return nil, fmt.Errorf("device not connected (%s); try again shortly", f.p.statusLine())
		case <-ctx.Done():
			return nil, errors.New("proxy shutting down")
		}
	}
}

// reopen opens a fresh channel for a non-primary mapping whose stream is missing, on the
// tunnel that up belongs to.
func (f *forwarder) reopen(up chan struct{}) error {
	f.mu.Lock()
	missing := f.stream == nil
	f.mu.Unlock()
	if !missing {
		return nil
	}
	tun := f.p.tunFor(up)
	if tun == nil {
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
	ch, err := f.p.openChannel(tun, f.m.Device)
	if err != nil {
		return fmt.Errorf("reopen channel to :%d: %w", f.m.Device, err)
	}
	// Attach only if this is still the current session and nothing attached meanwhile; the
	// check and the set happen under one lock so a redial's stream is never overwritten.
	f.p.mu.Lock()
	f.mu.Lock()
	ok := f.p.up == up && f.p.tun == tun && f.stream == nil
	if ok {
		f.stream = ch
	}
	f.mu.Unlock()
	f.p.mu.Unlock()
	if !ok {
		ch.Close()
		return nil
	}
	go f.readLoop(ch)
	return nil
}

// tunFor returns the live tunnel if up is still the current session's channel.
func (p *proxy) tunFor(up chan struct{}) tunnel {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.up != up {
		return nil
	}
	return p.tun
}

// openChannel serialises channel opens: the device handles one at a time, and Kalay
// refuses a second open while one is in flight.
func (p *proxy) openChannel(tun tunnel, port int) (io.ReadWriteCloser, error) {
	p.openMu.Lock()
	defer p.openMu.Unlock()
	return tun.OpenChannel(port)
}

// openFresh waits up to ClientWait for the tunnel and opens a new channel to port on it.
func (p *proxy) openFresh(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	deadline := time.NewTimer(p.opts.ClientWait)
	defer deadline.Stop()
	for {
		up := p.upChan()
		select {
		case <-up:
			tun := p.tunFor(up)
			if tun == nil {
				continue
			}
			return p.openChannel(tun, port)
		case <-deadline.C:
			return nil, fmt.Errorf("device not connected (%s); try again shortly", p.statusLine())
		case <-ctx.Done():
			return nil, errors.New("proxy shutting down")
		}
	}
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
			Attached: f.stream != nil || (f.perClient && st.State == "connected"),
			Client:   f.client != nil || f.active > 0,
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
