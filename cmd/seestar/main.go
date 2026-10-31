// Command seestar reaches a ZWO Seestar over the internet: it logs in through ZWO's cloud,
// opens the scope's Kalay P2P tunnel, and runs the scope's own services over it.
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/Praxeis/seestar-link/internal/cli"
	"github.com/Praxeis/seestar-link/internal/cmd/account"
	"github.com/Praxeis/seestar-link/internal/cmd/download"
	"github.com/Praxeis/seestar-link/internal/cmd/proxy"
	"github.com/Praxeis/seestar-link/internal/cmd/service"
	"github.com/Praxeis/seestar-link/internal/cmd/upload"
)

// version is set at release build time with -ldflags "-X main.version=v1.2.3".
var version = ""

type command struct {
	name, summary string
	run           func()
}

var commands = []command{
	{"login", "check your ZWO login and save the password", account.Login},
	{"logout", "remove a saved password", account.Logout},
	{"devices", "list the Seestars on your account", account.Devices},
	{"profile", "add, list, pick and edit scope profiles", account.Profile},
	{"proxy", "serve the scope's ports on this computer", proxy.Main},
	{"status", "show whether the proxy's tunnel is up", account.Status},
	{"service", "run the proxy in the background (install, start, stop, ...)", service.Main},
	{"download", "copy the scope's image archive to this computer", download.Main},
	{"upload", "copy files onto the scope's image share", upload.Main},
	{"version", "print the version", printVersion},
}

func usage() {
	fmt.Fprint(os.Stderr, `seestar - remote access to a ZWO Seestar over the internet

Usage:
  seestar <command> [flags]

Commands:
`)
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprint(os.Stderr, `
First run:
  seestar login                  save your ZWO login
  seestar profile add s30        pick a scope and name it
  seestar proxy                  serve it on 127.0.0.1:4700
  seestar service install        or keep it running in the background

Every command takes -profile NAME, and -h for its own help.
`)
}

func printVersion() {
	v := version
	if v == "" {
		v = "dev"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	fmt.Printf("seestar %s (%s/%s, %s)\n", v, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	switch name {
	case "-h", "-help", "--help", "help":
		usage()
		return
	case "-v", "-version", "--version":
		printVersion()
		return
	}
	for _, c := range commands {
		if c.name == name {
			// Each command parses os.Args as if it were its own program.
			cli.ProgName = "seestar " + name
			os.Args = append([]string{cli.ProgName}, os.Args[2:]...)
			c.run()
			return
		}
	}
	fmt.Fprintf(os.Stderr, "seestar: unknown command %q\n\n", name)
	usage()
	os.Exit(2)
}
