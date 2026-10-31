package proxy

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Praxeis/seestar-link/internal/cli"
	"github.com/Praxeis/seestar-link/internal/config"
)

// mappings collects repeated -p flags.
type mappings []Mapping

func (m *mappings) String() string { return "" }

func (m *mappings) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		mp, err := ParseMapping(part)
		if err != nil {
			return err
		}
		*m = append(*m, mp)
	}
	return nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `%[1]s exposes the scope's ports on this computer over the internet tunnel.

Ports are mapped Docker style, -p local:device, repeatable. All mappings share one
tunnel, since the device accepts a single remote session. While the proxy runs it holds
that session, so the Seestar app cannot connect remotely at the same time.

If the link drops, the proxy redials on its own (5s, 10s, 20s, 40s, then every 60s) and
reattaches every port. Clients only need to reconnect. A status page shows the link state.

Device ports: 4700 JSON-RPC, 32323 ASCOM Alpaca REST, 80 HTTP image server.

Usage:
  %[1]s [flags]

  %[1]s -p 4700:4700
  %[1]s -p 32323:32323 -p 8080:80
  %[1]s -profile s50                  use the ports saved in the profile

Flags:
`, cli.ProgName)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Environment:
  LISTEN_HOST
        Address to bind the listeners to (default 127.0.0.1).
%s
`, cli.ConnectEnvUsage)
}

// Main runs the proxy command line.
func Main() {
	var maps mappings
	flag.Var(&maps, "p", "port mapping local:device, repeatable (default: the profile's ports, else 4700:4700)")
	listen := flag.String("listen", "", "address to bind the mapped ports to (default 127.0.0.1)")
	status := flag.String("status", "", `status page address, or "off" (default: the profile's, else `+config.DefaultStatusAddr+`)`)
	noReconnect := flag.Bool("no-reconnect", false, "exit when the tunnel drops instead of redialling")
	noPrompt := flag.Bool("no-prompt", false, "fail instead of prompting for anything unset")
	cli.ParseFlags(usage)

	if err := run(context.Background(), maps, *listen, *status, !*noReconnect, cli.NoPrompt(*noPrompt), false); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// RunService runs the proxy for a background service: no prompting, always reconnecting,
// settings from the profile, until ctx is cancelled.
func RunService(ctx context.Context) error {
	return run(ctx, nil, "", "", true, true, true)
}

func run(ctx context.Context, maps mappings, listen, status string, reconnect, noPrompt, service bool) error {
	name, prof, _, err := cli.Profile()
	if err != nil {
		return err
	}
	if len(maps) == 0 {
		for _, s := range prof.Ports {
			if err := maps.Set(s); err != nil {
				return fmt.Errorf("profile %s: %w", name, err)
			}
		}
	}
	if len(maps) == 0 {
		maps = mappings{{Local: 4700, Device: 4700}}
	}
	if listen == "" {
		listen = cli.EnvOr("LISTEN_HOST", prof.Listen)
	}
	switch status {
	case "":
		status = prof.StatusAddr()
	case "off", "none":
		status = ""
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := cli.Resolve(noPrompt)
	// A service can start before the network is up, so it keeps trying to log in rather
	// than exiting; interactive runs report the problem straight away.
	for wait := 10 * time.Second; err != nil && service; wait = min(wait*2, 2*time.Minute) {
		logf("login failed: %v; retrying in %s", err, wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		cfg, err = cli.Resolve(noPrompt)
	}
	if err != nil {
		return err
	}

	return Run(ctx, cfg, Options{
		Profile:    name,
		Listen:     listen,
		Maps:       maps,
		StatusAddr: status,
		Reconnect:  reconnect,
		Logf:       logf,
	})
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
