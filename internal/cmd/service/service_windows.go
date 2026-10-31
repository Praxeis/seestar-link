//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	if src != exe {
		if err := copyFile(src, exe); err != nil {
			return fmt.Errorf("copy program to %s: %w", exe, err)
		}
	}

	// Copy this profile and the login to the machine location, readable by SYSTEM and
	// Administrators only, with the password under machine-scope DPAPI.
	md := machineDir()
	if err := os.MkdirAll(filepath.Join(md, "logs"), 0o700); err != nil {
		return err
	}
	if out, err := exec.Command("icacls", md, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F").CombinedOutput(); err != nil {
		return fmt.Errorf("restrict %s: %v: %s", md, err, out)
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
	// Remove the machine copy of the password; the profile and log are left for reference.
	config.SetDir(machineDir())
	secret.Delete(t.prof.Email)
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
	// Querying status does not need Administrator rights.
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(t.name)
	if err != nil {
		fmt.Printf("%s: not installed\n", t.display)
		return nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s\nLog: %s\n", t.display, stateNames[st.State], filepath.Join(machineDir(), "logs", t.profile+".log"))
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
