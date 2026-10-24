package vmm

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/skerrysoftware-rgb/skerry/internal/project"
)

// resources returns the CPUs and memory for a workspace VM: by default,
// enough for what the project runs (its services and apps; the VM only
// takes memory from the machine as it uses it), or SKERRY_CPUS and
// SKERRY_MEMORY ("4096" MiB, or "4g"), kept within what
// this machine can give: at most all its CPUs and half its memory.
func resources(proj project.Project) (cpus uint, memMiB uint64) {
	cpus, memMiB = CPUs, MemoryMiB
	if need := uint64(2048 + 512*len(proj.Services) + 768*len(proj.Apps)); need > memMiB {
		memMiB = need
	}
	if v := os.Getenv("SKERRY_CPUS"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "ignoring SKERRY_CPUS=%q\n", v)
		} else {
			cpus = uint(n)
		}
	}
	if v := os.Getenv("SKERRY_MEMORY"); v != "" {
		if n, ok := parseMiB(v); ok && n >= 1024 {
			memMiB = n
		} else {
			fmt.Fprintf(os.Stderr, "ignoring SKERRY_MEMORY=%q: use MiB (4096) or GiB (4g), at least 1g\n", v)
		}
	}
	cpus = min(cpus, uint(runtime.NumCPU()))
	if total := memTotal(); total > 0 {
		memMiB = min(memMiB, max(total>>20/2, 1024))
	}
	return cpus, memMiB
}

func parseMiB(v string) (uint64, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	mult := uint64(1)
	switch {
	case strings.HasSuffix(v, "g"):
		mult, v = 1024, strings.TrimSuffix(v, "g")
	case strings.HasSuffix(v, "m"):
		v = strings.TrimSuffix(v, "m")
	}
	n, err := strconv.ParseUint(v, 10, 32)
	return n * mult, err == nil
}
