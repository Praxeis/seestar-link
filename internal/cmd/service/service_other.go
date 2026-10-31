//go:build !windows && !darwin && !linux

package service

import (
	"context"
	"errors"

	"github.com/Praxeis/seestar-link/internal/cmd/proxy"
)

var errUnsupported = errors.New("service install is supported on Windows, macOS and Linux; run 'seestar proxy' under your own process supervisor")

func install(target) error   { return errUnsupported }
func uninstall(target) error { return errUnsupported }
func start(target) error     { return errUnsupported }
func stop(target) error      { return errUnsupported }
func status(target) error    { return errUnsupported }
func run() error             { return proxy.RunService(context.Background()) }
