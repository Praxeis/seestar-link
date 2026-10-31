// Package cli resolves a ConnectConfig from the environment, prompting for anything
// missing unless prompting is disabled.
package cli

import (
	"bufio"
	_ "embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/Praxeis/seestar-link/internal/config"
	"github.com/Praxeis/seestar-link/internal/secret"
	"github.com/Praxeis/seestar-link/kalay"
	"github.com/Praxeis/seestar-link/seestar"
)

// EnvOr returns the environment value for k, or def when unset.
func EnvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// EnvIntOr returns the environment value for k parsed as an int, or def when unset or unparseable.
func EnvIntOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// NoPrompt reports whether prompting is disabled, by flag or by SEESTAR_NO_PROMPT.
func NoPrompt(flag bool) bool { return flag || os.Getenv("SEESTAR_NO_PROMPT") != "" }

// Resolve builds a ConnectConfig. Each value is taken from the first place that has it:
// the environment (SEESTAR_EMAIL, SEESTAR_PASSWORD, SEESTAR_SN, SEESTAR_MODEL,
// SEESTAR_MASTER), then the selected profile and the saved password for its account, then
// an interactive prompt. With noPrompt set, a missing value is an error instead, so scripted
// runs and services fail rather than block. The returned config carries the access token,
// so Dial does not log in again.
func Resolve(noPrompt bool) (seestar.ConnectConfig, error) {
	var cfg seestar.ConnectConfig

	chRecord, err := clientHello()
	if err != nil {
		return cfg, err
	}

	_, prof, _, err := Profile()
	if err != nil {
		return cfg, err
	}

	email := firstOf(os.Getenv("SEESTAR_EMAIL"), prof.Email)
	password := os.Getenv("SEESTAR_PASSWORD")
	if email != "" && password == "" {
		switch pw, err := secret.Get(email); {
		case err == nil:
			password = pw
		case !errors.Is(err, secret.ErrNotFound):
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
	}
	if email == "" || password == "" {
		if noPrompt {
			return cfg, fmt.Errorf("no saved login: run 'seestar login' (or set SEESTAR_EMAIL and SEESTAR_PASSWORD)")
		}
		if email == "" {
			if email, err = ask("ZWO account email: "); err != nil {
				return cfg, err
			}
		}
		if password, err = askSecret("Password for " + email + ": "); err != nil {
			return cfg, err
		}
	}

	token, err := seestar.Login(email, password)
	if err != nil {
		return cfg, err
	}

	sn := firstOf(os.Getenv("SEESTAR_SN"), prof.Sn)
	model := firstOf(os.Getenv("SEESTAR_MODEL"), prof.Model)
	if sn == "" {
		if sn, model, err = selectDevice(token, noPrompt); err != nil {
			return cfg, err
		}
	}
	if model == "" {
		model = "Seestar S30 Pro"
	}

	return seestar.ConnectConfig{
		Email:             email,
		Password:          password,
		Token:             token,
		DeviceModel:       model,
		DeviceSn:          sn,
		Master:            firstOf(os.Getenv("SEESTAR_MASTER"), prof.Master, DefaultMaster),
		ClientHelloRecord: chRecord,
	}, nil
}

// DefaultMaster is the Kalay rendezvous master used when nothing overrides it.
const DefaultMaster = "119.45.181.137:3478"

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Shared flags, registered by ParseFlags on every command.
var (
	profileFlag   string
	configDirFlag string
	logFlag       string
)

// Profile returns the profile selected by -profile or SEESTAR_PROFILE, else the default.
// ok is false when no profiles are configured, which is not an error: the environment and
// prompts still work as they did before profiles existed.
func Profile() (name string, p config.Profile, ok bool, err error) {
	c, err := config.Load()
	if err != nil {
		return "", p, false, err
	}
	return c.Pick(firstOf(profileFlag, os.Getenv("SEESTAR_PROFILE")))
}

// ProfileName returns the explicitly requested profile name, if any.
func ProfileName() string { return firstOf(profileFlag, os.Getenv("SEESTAR_PROFILE")) }

// SelectDevice logs in with token and returns the chosen device. It is exported for the
// profile setup commands.
func SelectDevice(token string) (seestar.Device, error) {
	devices, err := seestar.ListDevices(token, "")
	if err != nil {
		return seestar.Device{}, err
	}
	sn, _, err := pickDevice(devices, false)
	if err != nil {
		return seestar.Device{}, err
	}
	for _, d := range devices {
		if d.Sn == sn {
			return d, nil
		}
	}
	return seestar.Device{}, fmt.Errorf("device %s vanished from the list", sn)
}

// selectDevice lists the account's devices and returns the chosen serial and model. A sole
// device is taken without asking.
func selectDevice(token string, noPrompt bool) (sn, model string, err error) {
	devices, err := seestar.ListDevices(token, "")
	if err != nil {
		return "", "", err
	}
	return pickDevice(devices, noPrompt)
}

// pickDevice chooses from a device list, prompting when there is more than one.
func pickDevice(devices []seestar.Device, noPrompt bool) (sn, model string, err error) {
	switch len(devices) {
	case 0:
		return "", "", fmt.Errorf("no devices registered to this account")
	case 1:
		fmt.Fprintf(os.Stderr, "device: %s\n", devices[0].Label())
		return devices[0].Sn, devices[0].Model, nil
	}
	if noPrompt {
		var sns []string
		for _, d := range devices {
			sns = append(sns, d.Sn)
		}
		return "", "", fmt.Errorf("account has %d devices; set SEESTAR_SN to one of: %s",
			len(devices), strings.Join(sns, ", "))
	}
	for i, d := range devices {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, d.Label())
	}
	for {
		s, err := ask(fmt.Sprintf("Device [1-%d]: ", len(devices)))
		if err != nil {
			return "", "", err
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(s))
		if convErr == nil && n >= 1 && n <= len(devices) {
			return devices[n-1].Sn, devices[n-1].Model, nil
		}
		fmt.Fprintln(os.Stderr, "  not a listed choice")
	}
}

// embeddedClientHello is a captured DTLS 1.2 ClientHello, replayed during the handshake.
// It is a standard boringssl ClientHello with no device, account, or secret content, so it
// is a fixed product-wide constant. SEESTAR_CH overrides it with a file of the same 570-hex
// form.
//
//go:embed clienthello.hex
var embeddedClientHello string

// clientHello returns the DTLS ClientHello record to replay, from SEESTAR_CH if set,
// otherwise the embedded capture.
func clientHello() ([]byte, error) {
	chHex := embeddedClientHello
	src := "embedded ClientHello"
	if path := os.Getenv("SEESTAR_CH"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("SEESTAR_CH: %w", err)
		}
		chHex, src = string(b), "SEESTAR_CH ("+path+")"
	}
	if len(chHex) < 570 {
		return nil, fmt.Errorf("%s: need the 570-hex ClientHello record, got %d bytes", src, len(chHex))
	}
	wire, err := hex.DecodeString(chHex[:570])
	if err != nil {
		return nil, fmt.Errorf("%s: not valid hex: %w", src, err)
	}
	if len(wire) < 28 {
		return nil, fmt.Errorf("%s: decoded record too short (%d bytes, need >28)", src, len(wire))
	}
	return kalay.DecodePayloadData(wire)[28:], nil
}

// ask prompts on stderr and reads a line from stdin, so prompts stay out of piped stdout.
func ask(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// askSecret reads without echoing when stdin is a terminal, and falls back to a plain read
// when it is not (a pipe or here-doc).
func askSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return ask(prompt)
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
}

// Ask prompts on stderr and reads a line from stdin.
func Ask(prompt string) (string, error) { return ask(prompt) }

// AskSecret prompts and reads without echo when stdin is a terminal.
func AskSecret(prompt string) (string, error) { return askSecret(prompt) }

// ConnectEnvUsage documents the environment Resolve reads. Commands append their own.
const ConnectEnvUsage = `  SEESTAR_PROFILE
        Profile to use, same as -profile. Defaults to 'seestar profile use'.
  SEESTAR_EMAIL, SEESTAR_PASSWORD
        ZWO account. Override the profile and the saved login.
  SEESTAR_SN
        Device serial. Overrides the profile; chosen from the account when unset.
  SEESTAR_MODEL
        Device model. Taken from the profile or the device list when unset.
  SEESTAR_MASTER
        Kalay rendezvous master (default ` + DefaultMaster + `).
  SEESTAR_CONFIG_DIR
        Configuration directory, same as -config-dir.
  SEESTAR_CH
        Override the embedded DTLS ClientHello with a 570-hex file (rarely needed).
  SEESTAR_NO_PROMPT
        Set to disable prompting, same as -no-prompt.
  KALAY_TUN_DEBUG
        Set for tunnel and RDT tracing on stderr.`

// ProgName is the name commands use in their usage text and errors. The seestar
// dispatcher sets it to "seestar <command>".
var ProgName = strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")

// ParseFlags registers the shared flags (-profile, -config-dir, -log), parses the command
// line, prints usage for -h/--help and exits 0. A genuine flag error prints usage and
// exits 2.
func ParseFlags(usage func()) {
	flag.CommandLine.Init(ProgName, flag.ContinueOnError)
	flag.StringVar(&profileFlag, "profile", "", "profile to use (see 'seestar profile list')")
	flag.StringVar(&configDirFlag, "config-dir", "", "configuration directory (default: your user config folder)")
	flag.StringVar(&logFlag, "log", "", "append output to this file instead of the console")
	flag.CommandLine.SetOutput(io.Discard) // suppress the package's own error/usage output
	flag.Usage = func() {}
	err := flag.CommandLine.Parse(os.Args[1:])
	if err == nil {
		applyShared()
		return
	}
	flag.CommandLine.SetOutput(os.Stderr)
	usage()
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
	os.Exit(2)
}

// applyShared acts on the shared flags once parsed.
func applyShared() {
	if configDirFlag != "" {
		config.SetDir(configDirFlag)
	}
	if logFlag != "" {
		if err := RedirectOutput(logFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

// RedirectOutput sends everything the process prints to path, appending.
func RedirectOutput(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("log: %w", err)
		}
	}
	// Keep the log bounded: past 5 MB it is rotated to path.1, replacing the previous one.
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("log: %w", err)
	}
	os.Stdout, os.Stderr = f, f
	return nil
}
