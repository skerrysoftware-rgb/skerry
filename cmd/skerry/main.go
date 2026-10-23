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
	code := cli.Main(ctx, os.Args[1:], cli.Env{
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Interactive: interactive,
		Getenv:      os.Getenv,
		Getwd:       os.Getwd,
	})
	stop()
	os.Exit(code)
}
