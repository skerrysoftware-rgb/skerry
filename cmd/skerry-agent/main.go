//go:build linux

// Command skerry-agent is the guest agent: PID 1 in every workspace VM.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/agent"
)

func main() {
	var err error
	switch {
	case os.Getpid() == 1 && len(os.Args) == 1:
		err = agent.Stage1() // /init in the initramfs
	case len(os.Args) == 2 && os.Args[1] == "pid1":
		err = agent.PID1()
	case len(os.Args) == 2 && os.Args[1] == "serve":
		err = agent.Serve()
	case len(os.Args) == 2 && os.Args[1] == "services":
		err = agent.RunServices()
	case len(os.Args) == 3 && os.Args[1] == "wait":
		var d time.Duration
		if d, err = time.ParseDuration(os.Args[2]); err == nil {
			err = agent.WaitServices(d)
		}
	default:
		fmt.Fprintln(os.Stderr, "skerry-agent runs inside workspace VMs; it is started by the kernel")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "skerry-agent: %v\n", err)
	}
	if os.Getpid() != 1 && err == nil {
		os.Exit(0)
	}
	if os.Getpid() == 1 {
		// PID 1 must never exit: the kernel would panic. Leave the console
		// readable for the host and wait to be stopped.
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(1)
}
