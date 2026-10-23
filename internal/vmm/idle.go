package vmm

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// defaultIdleStop is how long a workspace runs with nothing using it before
// its VM stops. SKERRY_IDLE_STOP overrides it ("45m"; "off" never stops).
const defaultIdleStop = 30 * time.Minute

// activity counts what is using a workspace: commands run through skerry
// (Claude's included) and forwarded connections.
type activity struct {
	mu     sync.Mutex
	active int
	last   time.Time
}

func newActivity() *activity { return &activity{last: time.Now()} }

// begin marks something starting to use the workspace; call the returned
// function when it ends.
func (a *activity) begin() func() {
	a.mu.Lock()
	a.active++
	a.last = time.Now()
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.active--
			a.last = time.Now()
			a.mu.Unlock()
		})
	}
}

// idleFor reports how long nothing has used the workspace.
func (a *activity) idleFor() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active > 0 {
		return 0
	}
	return time.Since(a.last)
}

// idleStop reads the idle timeout; zero means never.
func idleStop() time.Duration {
	v := os.Getenv("SKERRY_IDLE_STOP")
	switch v {
	case "":
		return defaultIdleStop
	case "off", "0":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute {
		fmt.Fprintf(os.Stderr, "ignoring SKERRY_IDLE_STOP=%q: use a duration of a minute or more, or off\n", v)
		return defaultIdleStop
	}
	return d
}

// whenIdle closes the returned channel once the workspace has been idle for
// timeout, checking every so often, until stop is closed.
func (a *activity) whenIdle(timeout time.Duration, stop <-chan struct{}) <-chan struct{} {
	idle := make(chan struct{})
	if timeout <= 0 {
		return idle
	}
	go func() {
		t := time.NewTicker(min(timeout/4, time.Minute))
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if a.idleFor() >= timeout {
					close(idle)
					return
				}
			}
		}
	}()
	return idle
}
