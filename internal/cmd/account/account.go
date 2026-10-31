// Package account implements the setup commands: login, logout, devices, profile and status.
package account

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Praxeis/seestar-link/internal/cli"
	"github.com/Praxeis/seestar-link/internal/cmd/proxy"
	"github.com/Praxeis/seestar-link/internal/config"
	"github.com/Praxeis/seestar-link/internal/secret"
	"github.com/Praxeis/seestar-link/seestar"
)

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func simpleUsage(text string) func() {
	return func() {
		fmt.Fprintf(os.Stderr, text, cli.ProgName)
		fmt.Fprintln(os.Stderr, "\nFlags:")
		flag.PrintDefaults()
	}
}

// login verifies credentials with ZWO and saves the password. With email empty it uses the
// selected profile's account, else asks. It returns the email and an access token.
func login(email string, forcePrompt bool) (string, string, error) {
	if email == "" {
		if _, p, ok, err := cli.Profile(); err == nil && ok {
			email = p.Email
		}
	}
	if email == "" {
		email = os.Getenv("SEESTAR_EMAIL")
	}
	var err error
	if email == "" {
		if email, err = cli.Ask("ZWO account email: "); err != nil {
			return "", "", err
		}
	}
	if email == "" {
		return "", "", errors.New("no email given")
	}
	if !forcePrompt {
		if pw, err := secret.Get(email); err == nil {
			tok, lerr := seestar.Login(email, pw)
			if lerr == nil {
				return email, tok, nil
			}
			fmt.Fprintf(os.Stderr, "saved password for %s no longer works: %v\n", email, lerr)
		}
	}
	pw, err := cli.AskSecret("Password for " + email + ": ")
	if err != nil {
		return "", "", err
	}
	tok, err := seestar.Login(email, pw)
	if err != nil {
		return "", "", fmt.Errorf("login failed: %w", err)
	}
	if err := secret.Set(email, pw); err != nil {
		return "", "", fmt.Errorf("logged in, but could not save the password: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Logged in as %s. Password saved in %s.\n", email, secret.Backend())
	return email, tok, nil
}

// Login is 'seestar login'.
func Login() {
	email := flag.String("email", "", "ZWO account email (default: the profile's account)")
	cli.ParseFlags(simpleUsage(`%s checks your ZWO login and saves the password, so later commands and the
background service can connect without asking.

Usage:
  %[1]s [-email you@example.com]
`))
	if _, _, err := login(*email, true); err != nil {
		fail(err)
	}
}

// Logout is 'seestar logout'.
func Logout() {
	email := flag.String("email", "", "ZWO account email (default: the profile's account)")
	cli.ParseFlags(simpleUsage(`%s removes the saved password for an account.

Usage:
  %[1]s [-email you@example.com]
`))
	e := *email
	if e == "" {
		_, p, ok, err := cli.Profile()
		if err != nil {
			fail(err)
		}
		if !ok {
			fail(errors.New("no profile selected; pass -email"))
		}
		e = p.Email
	}
	if err := secret.Delete(e); err != nil {
		fail(err)
	}
	fmt.Printf("Removed the saved password for %s.\n", e)
}

// Devices is 'seestar devices'.
func Devices() {
	email := flag.String("email", "", "ZWO account email (default: the profile's account)")
	cli.ParseFlags(simpleUsage(`%s lists the Seestars on your ZWO account, including ones shared with you.

Usage:
  %[1]s [-email you@example.com]
`))
	_, tok, err := login(*email, false)
	if err != nil {
		fail(err)
	}
	devs, err := seestar.ListDevices(tok, "")
	if err != nil {
		fail(err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSERIAL\tMODEL\tSHARED")
	for _, d := range devs {
		name := d.Alias
		if name == "" {
			name = d.SSID
		}
		shared := ""
		if d.SharedWithMe() {
			shared = "with you"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, d.Sn, d.Model, shared)
	}
	w.Flush()
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if _, err := proxy.ParseMapping(part); err != nil {
			return err
		}
		*s = append(*s, strings.TrimSpace(part))
	}
	return nil
}

const profileHelp = `%s manages named scopes, so you can switch between Seestars by name.

Usage:
  %[1]s add NAME [-email addr] [-p local:device ...] [-status addr] [-listen addr] [-default]
        Log in (or reuse the saved login), pick a device, and save it as NAME.
  %[1]s set NAME [-p local:device ...] [-status addr] [-listen addr]
        Change a profile's proxy ports or addresses. -p replaces the list.
  %[1]s list
  %[1]s show NAME
  %[1]s use NAME        make NAME the default
  %[1]s remove NAME

Each profile gets its own local ports and status address, so two scopes can be proxied
at once. Unless you pass -p, the first profile uses 4700:4700 and later ones move the
local side up by 10 (4710:4700, 4720:4700, ...).
`

// Profile is 'seestar profile'.
func Profile() {
	if len(os.Args) < 2 || strings.HasPrefix(os.Args[1], "-") {
		fmt.Fprintf(os.Stderr, profileHelp, cli.ProgName)
		if len(os.Args) >= 2 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "-help") {
			os.Exit(0)
		}
		os.Exit(2)
	}
	action := os.Args[1]
	prog := cli.ProgName + " " + action
	os.Args = append([]string{prog}, os.Args[2:]...)
	cli.ProgName = prog

	var ports stringList
	var status, listen *string
	var makeDefault *bool
	var addEmail *string
	if action == "add" || action == "set" {
		flag.Var(&ports, "p", "proxy port mapping local:device, repeatable")
		status = flag.String("status", "", "status page address (host:port)")
		listen = flag.String("listen", "", "address to bind the proxy ports to")
	}
	if action == "add" {
		makeDefault = flag.Bool("default", false, "make this the default profile")
		addEmail = flag.String("email", "", "ZWO account email (default: the account of the current default profile)")
	}
	usage := func() { fmt.Fprintf(os.Stderr, profileHelp, "seestar profile") }
	cli.ParseFlags(usage)
	name := ""
	if pos := cli.ParseRest(usage); len(pos) > 0 {
		name = pos[0]
		if len(pos) > 1 {
			fail(fmt.Errorf("unexpected arguments: %v", pos[1:]))
		}
	}

	c, err := config.Load()
	if err != nil {
		fail(err)
	}
	need := func() {
		if name == "" {
			fail(fmt.Errorf("%s needs a profile name", action))
		}
		if action != "add" {
			if _, ok := c.Profiles[name]; !ok {
				fail(fmt.Errorf("no profile named %q (have: %v)", name, c.Names()))
			}
		}
	}

	switch action {
	case "add":
		need()
		if err := config.ValidName(name); err != nil {
			fail(err)
		}
		if _, exists := c.Profiles[name]; exists {
			fail(fmt.Errorf("profile %q already exists; use 'seestar profile set' or remove it first", name))
		}
		email, tok, err := login(*addEmail, false)
		if err != nil {
			fail(err)
		}
		d, err := cli.SelectDevice(tok)
		if err != nil {
			fail(err)
		}
		p := config.Profile{Email: email, Sn: d.Sn, Model: d.Model, Alias: d.Alias, Listen: *listen, Status: *status}
		if p.Alias == "" {
			p.Alias = d.SSID
		}
		p.Ports = []string(ports)
		if len(p.Ports) == 0 {
			p.Ports = []string{fmt.Sprintf("%d:4700", freeLocal(c, 4700))}
		}
		if p.Status == "" {
			p.Status = freeStatus(c)
		}
		c.Profiles[name] = p
		if *makeDefault || c.Default == "" {
			c.Default = name
		}
		if err := c.Save(); err != nil {
			fail(err)
		}
		fmt.Printf("Saved profile %q: %s, ports %s, status %s.\n", name, p.Label(), strings.Join(p.Ports, " "), p.StatusAddr())
		if c.Default == name {
			fmt.Println("It is the default profile.")
		}
	case "set":
		need()
		p := c.Profiles[name]
		if len(ports) > 0 {
			p.Ports = []string(ports)
		}
		if *status != "" {
			p.Status = *status
		}
		if *listen != "" {
			p.Listen = *listen
		}
		c.Profiles[name] = p
		if err := c.Save(); err != nil {
			fail(err)
		}
		fmt.Printf("Updated profile %q.\n", name)
	case "list", "ls":
		if len(c.Profiles) == 0 {
			fmt.Println("No profiles yet. Add one with: seestar profile add NAME")
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "\tNAME\tSCOPE\tACCOUNT\tPORTS\tSTATUS")
		for _, n := range c.Names() {
			p := c.Profiles[n]
			mark := ""
			if n == c.Default {
				mark = "*"
			}
			ports := strings.Join(p.Ports, " ")
			if ports == "" {
				ports = "4700:4700"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", mark, n, p.Label(), p.Email, ports, p.StatusAddr())
		}
		w.Flush()
	case "show":
		need()
		p := c.Profiles[name]
		fmt.Printf("name:    %s\nscope:   %s\nmodel:   %s\naccount: %s\nports:   %s\nlisten:  %s\nstatus:  %s\ndefault: %v\n",
			name, p.Label(), p.Model, p.Email, strings.Join(p.Ports, " "), orDefault(p.Listen, "127.0.0.1"), p.StatusAddr(), name == c.Default)
	case "use":
		need()
		c.Default = name
		if err := c.Save(); err != nil {
			fail(err)
		}
		fmt.Printf("Default profile is now %q.\n", name)
	case "remove", "rm":
		need()
		delete(c.Profiles, name)
		if c.Default == name {
			c.Default = ""
		}
		if err := c.Save(); err != nil {
			fail(err)
		}
		fmt.Printf("Removed profile %q. The saved password for its account was kept; 'seestar logout -email ...' removes it.\n", name)
	default:
		fmt.Fprintf(os.Stderr, profileHelp, "seestar profile")
		os.Exit(2)
	}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// freeLocal returns base, or base plus a multiple of 10, whichever no other profile uses
// as a local port.
func freeLocal(c *config.Config, base int) int {
	used := map[int]bool{}
	for _, p := range c.Profiles {
		ports := p.Ports
		if len(ports) == 0 {
			ports = []string{"4700:4700"}
		}
		for _, s := range ports {
			if m, err := proxy.ParseMapping(s); err == nil {
				used[m.Local] = true
			}
		}
	}
	for l := base; ; l += 10 {
		if !used[l] {
			return l
		}
	}
}

// freeStatus returns a status address no other profile uses, counting up from the default.
func freeStatus(c *config.Config) string {
	used := map[string]bool{}
	for _, p := range c.Profiles {
		used[p.StatusAddr()] = true
	}
	host, portStr, _ := net.SplitHostPort(config.DefaultStatusAddr)
	port, _ := strconv.Atoi(portStr)
	for {
		a := net.JoinHostPort(host, strconv.Itoa(port))
		if !used[a] {
			return a
		}
		port++
	}
}

// Status is 'seestar status'.
func Status() {
	all := flag.Bool("all", false, "show every profile")
	asJSON := flag.Bool("json", false, "print the raw status JSON")
	cli.ParseFlags(simpleUsage(`%s shows whether a running proxy (or service) has its tunnel up.

Usage:
  %[1]s [-profile NAME | -all] [-json]
`))
	c, err := config.Load()
	if err != nil {
		fail(err)
	}
	type target struct{ name, addr string }
	var targets []target
	if *all {
		for _, n := range c.Names() {
			targets = append(targets, target{n, c.Profiles[n].StatusAddr()})
		}
	} else {
		n, p, _, err := c.Pick(cli.ProfileName())
		if err != nil {
			fail(err)
		}
		targets = append(targets, target{n, p.StatusAddr()})
	}
	if len(targets) == 0 {
		targets = append(targets, target{"", config.DefaultStatusAddr})
	}
	down := false
	for _, t := range targets {
		st, err := proxy.FetchStatus(t.addr)
		label := t.name
		if label == "" {
			label = t.addr
		}
		if err != nil {
			fmt.Printf("%s: not running (nothing answering on %s)\n", label, t.addr)
			down = true
			continue
		}
		if *asJSON {
			b, _ := jsonIndent(st)
			fmt.Println(string(b))
			continue
		}
		fmt.Printf("%s: %s", label, st.State)
		switch st.State {
		case "connected":
			fmt.Printf(" to %s (%s) for %s", st.Device, st.Model, st.Uptime)
		case "waiting":
			if st.NextTry != nil {
				fmt.Printf(", next try in %s", time.Until(*st.NextTry).Round(time.Second))
			}
		}
		fmt.Println()
		if st.LastError != "" && st.State != "connected" {
			fmt.Printf("  last error: %s\n", st.LastError)
		}
		fmt.Printf("  connects: %d, attempts: %d\n", st.Connects, st.Attempts)
		for _, m := range st.Mappings {
			state := "detached"
			if m.Attached {
				state = "ready"
			}
			if m.Client {
				state = "in use"
			}
			fmt.Printf("  %s:%d -> device :%d  %s, %d client(s), %s up / %s down\n",
				st.ListenHost, m.Local, m.Device, state, m.Clients, human(m.BytesUp), human(m.BytesDown))
		}
		if st.State != "connected" {
			down = true
		}
	}
	if down {
		os.Exit(3)
	}
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
