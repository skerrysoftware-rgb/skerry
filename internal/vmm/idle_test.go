package vmm

import (
	"github.com/skerrysoftware-rgb/skerry/internal/project"
	"testing"
	"time"
)

func TestIdleOnlyWhenNothingRuns(t *testing.T) {
	a := newActivity()
	a.last = time.Now().Add(-time.Hour)
	end := a.begin()
	if a.idleFor() != 0 {
		t.Error("idle while a command runs")
	}
	end()
	end() // ending twice is harmless
	if d := a.idleFor(); d > time.Second {
		t.Errorf("idle for %s right after a command ended", d)
	}
	a.mu.Lock()
	a.last = time.Now().Add(-time.Hour)
	a.mu.Unlock()
	stop := make(chan struct{})
	defer close(stop)
	select {
	case <-a.whenIdle(4*time.Millisecond, stop):
	case <-time.After(5 * time.Second):
		t.Fatal("never reported idle")
	}
}

func TestIdleStopSetting(t *testing.T) {
	for v, want := range map[string]time.Duration{"": defaultIdleStop, "off": 0, "0": 0, "45m": 45 * time.Minute, "5s": defaultIdleStop, "junk": defaultIdleStop} {
		t.Setenv("SKERRY_IDLE_STOP", v)
		if got := idleStop(); got != want {
			t.Errorf("SKERRY_IDLE_STOP=%q: %s, want %s", v, got, want)
		}
	}
}

func TestResources(t *testing.T) {
	t.Setenv("SKERRY_CPUS", "2")
	t.Setenv("SKERRY_MEMORY", "2g")
	if cpus, mem := resources(project.Project{}); cpus != 2 || mem != 2048 {
		t.Errorf("resources(project.Project{}) = %d, %d", cpus, mem)
	}
	t.Setenv("SKERRY_MEMORY", "100000g")
	if _, mem := resources(project.Project{}); mem<<20 > memTotal()/2 && memTotal() > 0 {
		t.Errorf("memory %d MiB is more than half the machine", mem)
	}
	t.Setenv("SKERRY_MEMORY", "512")
	if _, mem := resources(project.Project{}); mem != MemoryMiB {
		t.Errorf("a too-small setting was used: %d", mem)
	}
}
