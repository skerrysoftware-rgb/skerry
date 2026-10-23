package agent

import (
	"fmt"
	"syscall"
)

func fmtSscan(s string, v *int) (int, error) { return fmt.Sscan(s, v) }

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
