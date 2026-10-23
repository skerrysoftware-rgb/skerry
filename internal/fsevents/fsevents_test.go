//go:build darwin

package fsevents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchSeesContentChanges(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []string, 16)
	w, err := Watch(dir, 0.05, func(p []string) { got <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	time.Sleep(200 * time.Millisecond)
	file := filepath.Join(dir, "app.js")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case paths := <-got:
			if strings.Contains(strings.Join(paths, "\n"), "app.js") {
				return
			}
		case <-deadline:
			t.Fatal("no event for a new file")
		}
	}
}

// Closing while events are being delivered must not leave a callback to
// run afterwards (which would use a deleted handle and crash the runner).
func TestCloseWhileDelivering(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	var after atomic.Int64
	for i := 0; i < 100; i++ {
		var closed atomic.Bool
		w, err := Watch(dir, 0, func([]string) {
			if closed.Load() {
				after.Add(1)
			}
			time.Sleep(200 * time.Microsecond)
		})
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = os.WriteFile(filepath.Join(dir, fmt.Sprint(j%50)), []byte{byte(j)}, 0o644)
			}
		}()
		time.Sleep(time.Duration(5+i%20) * time.Millisecond)
		w.Close()
		closed.Store(true)
		close(stop)
		<-done
	}
	time.Sleep(200 * time.Millisecond)
	if n := after.Load(); n != 0 {
		t.Errorf("%d callbacks ran after Close returned", n)
	}
}
