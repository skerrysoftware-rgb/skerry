// Command skerry gives every checkout an isolated, lightweight VM.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/skerrysoftware-rgb/skerry/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	interactive := false
	if info, err := os.Stdin.Stat(); err == nil {
		interactive = info.Mode()&os.ModeCharDevice != 0
	}
	env := cli.Env{
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Interactive: interactive,
		Getenv:      os.Getenv,
		Getwd:       os.Getwd,
	}
	var code int
	if cli.IsShim(os.Args[0]) {
		// Invoked as node, npm, ...: run it in the workspace VM, or the real one.
		code = cli.Shim(ctx, os.Args[0], os.Args[1:], env)
	} else {
		code = cli.Main(ctx, os.Args[1:], env)
	}
	stop()
	os.Exit(code)
}
