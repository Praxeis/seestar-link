//go:build windows

package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/Praxeis/seestar-link/internal/cli"
	"github.com/Praxeis/seestar-link/internal/cmd/proxy"
	"github.com/Praxeis/seestar-link/internal/config"
	"github.com/Praxeis/seestar-link/internal/secret"
)

// machineDir holds the configuration and logs a Windows service reads. A service runs as
// LocalSystem, which cannot see the installing user's profile folder or user-scope DPAPI.
func machineDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, config.AppName)
}

func programDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, config.AppName)
}

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, errors.New("managing services needs an Administrator terminal: right-click Terminal or PowerShell, choose 'Run as administrator', and run this again")
	}
	return m, err
}

func install(t target) error {
	pw, err := savedPassword(t)
	if err != nil {
		return err
	}
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	// Replace an existing install so a reinstall picks up new settings and a new binary.
	if s, err := m.OpenService(t.name); err == nil {
		stopAndWait(s)
		s.Delete()
		s.Close()
		time.Sleep(time.Second)
	}

	// Copy the program somewhere stable.
	src, err := executable()
	if err != nil {
		return err
	}
	exe := filepath.Join(programDir(), "seestar.exe")
	if err := os.MkdirAll(programDir(), 0o755); err != nil {
		return err
	}
	if src != exe && !sameContents(src, exe) {
		if err := replaceProgram(src, exe); err != nil {
			return fmt.Errorf("copy program to %s: %w", exe, err)
		}
	}

	// Copy this profile and the login to the machine location, readable by SYSTEM and
	// Administrators only, with the password under machine-scope DPAPI.
	md := machineDir()
	if err := os.MkdirAll(filepath.Join(md, "logs"), 0o700); err != nil {
		return err
	}
	// The machine-scope password can be decrypted by any process on this computer, so the
	// folder's permissions are its protection. Take ownership (in case someone else created
	// the folder first), clear every explicit entry, then allow only SYSTEM and Administrators.
	for _, args := range [][]string{
		{md, "/setowner", "*S-1-5-32-544", "/T", "/C", "/Q"},
		{md, "/reset", "/T", "/C", "/Q"},
		{md, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "/Q"},
	} {
		if out, err := exec.Command("icacls", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("restrict %s: icacls %v: %v: %s", md, args[1:], err, out)
		}
	}
	userDir, err := config.Dir()
	if err != nil {
		return err
	}
	config.SetDir(md)
	mc, err := config.Load()
	if err != nil {
		return err
	}
	mc.Profiles[t.profile] = t.prof
	if mc.Default == "" {
		mc.Default = t.profile
	}
	if err := mc.Save(); err != nil {
		return err
	}
	secret.UseMachineScope(true)
	if err := secret.Set(t.prof.Email, pw); err != nil {
		return err
	}
	config.SetDir(userDir)
	secret.UseMachineScope(false)

	s, err := m.CreateService(t.name, exe, mgr.Config{
		DisplayName:      t.display,
		Description:      "Keeps a tunnel to the Seestar '" + t.profile + "' open and serves its ports on this computer.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true, // after networking is up
	}, "service", "run", "-profile", t.profile, "-config-dir", md, "-log", filepath.Join(md, "logs", t.profile+".log"))
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 24*60*60)
	// Count an exit with an error as a failure too, not only a crash.
	s.SetRecoveryActionsOnNonCrashFailures(true)
	if err := s.Start(); err != nil {
		return fmt.Errorf("installed, but starting failed: %w", err)
	}
	fmt.Printf("Installed and started %q.\nProgram: %s\nLog: %s\nCheck it with: seestar status -profile %s\n",
		t.display, exe, filepath.Join(md, "logs", t.profile+".log"), t.profile)
	return nil
}

// replaceProgram installs src as dst. Another profile's service may be running dst, and a
// running program cannot be overwritten on Windows, but it can be renamed: the old copy is
// moved aside, and those services pick up the new one at their next restart.
func replaceProgram(src, dst string) error {
	old, _ := filepath.Glob(dst + ".old-*")
	for _, o := range old {
		os.Remove(o) // fails harmlessly while still running
	}
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, fmt.Sprintf("%s.old-%d", dst, time.Now().Unix())); err != nil {
			return err
		}
	}
	return copyFile(src, dst)
}

// sameContents reports whether two files hold identical bytes.
func sameContents(a, b string) bool {
	ha, errA := fileHash(a)
	hb, errB := fileHash(b)
	return errA == nil && errB == nil && ha == hb
}

func fileHash(path string) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func stopAndWait(s *mgr.Service) {
	st, err := s.Control(svc.Stop)
	if err != nil {
		return
	}
	for i := 0; i < 30 && st.State != svc.Stopped; i++ {
		time.Sleep(500 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return
		}
	}
}

func open(t target) (*mgr.Mgr, *mgr.Service, error) {
	m, err := connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(t.name)
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("%s is not installed (seestar service install -profile %s)", t.display, t.profile)
	}
	return m, s, nil
}

func uninstall(t target) error {
	m, s, err := open(t)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	stopAndWait(s)
	err = s.Delete()
	s.Close()
	if err != nil {
		return err
	}
	// Drop this profile from the machine copy, and its password only if no other installed
	// profile uses the same account. Logs are left for reference.
	config.SetDir(machineDir())
	if mc, err := config.Load(); err == nil {
		delete(mc.Profiles, t.profile)
		if mc.Default == t.profile {
			mc.Default = ""
		}
		mc.Save()
		shared := false
		for _, p := range mc.Profiles {
			if strings.EqualFold(p.Email, t.prof.Email) {
				shared = true
			}
		}
		if !shared {
			secret.Delete(t.prof.Email)
		}
	}
	fmt.Printf("Removed %q. Logs remain in %s.\n", t.display, filepath.Join(machineDir(), "logs"))
	return nil
}

func start(t target) error {
	m, s, err := open(t)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err := s.Start(); err != nil {
		return err
	}
	fmt.Printf("Started %q.\n", t.display)
	return nil
}

func stop(t target) error {
	m, s, err := open(t)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	stopAndWait(s)
	fmt.Printf("Stopped %q.\n", t.display)
	return nil
}

var stateNames = map[svc.State]string{
	svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping",
	svc.Running: "running", svc.ContinuePending: "resuming", svc.PausePending: "pausing", svc.Paused: "paused",
}

func status(t target) error {
	// Query with the minimum rights, so this works from a normal terminal; mgr.Connect
	// asks for full access, which only an Administrator gets.
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(scm)
	name, err := windows.UTF16PtrFromString(t.name)
	if err != nil {
		return err
	}
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		fmt.Printf("%s: not installed\n", t.display)
		return nil
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return err
	}
	state, ok := stateNames[svc.State(st.CurrentState)]
	if !ok {
		state = fmt.Sprintf("state %d", st.CurrentState)
	}
	fmt.Printf("%s: %s\nLog: %s\n", t.display, state, filepath.Join(machineDir(), "logs", t.profile+".log"))
	return nil
}

// run is the service entry point. Under the service manager it reports state and stops on
// request; started by hand it simply runs the proxy until Ctrl-C.
func run() error {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isSvc {
		return proxy.RunService(context.Background())
	}
	name := config.AppName
	if p := cli.ProfileName(); p != "" {
		name += "-" + p
	}
	return svc.Run(name, handler{})
}

type handler struct{}

func (handler) Execute(args []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- proxy.RunService(ctx) }()
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			cancel()
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				return false, 1 // the recovery actions restart the service
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				st <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		}
	}
}
