package image

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A connection that stalls is dropped and the download tried again; a
// slow one that keeps sending isn't; a missing file isn't retried.
func TestDownloadRetriesStalls(t *testing.T) {
	defer func(s time.Duration) { downloadStall = s }(downloadStall)
	downloadStall = 300 * time.Millisecond

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch {
		case r.URL.Path == "/missing":
			http.NotFound(w, r)
		case r.URL.Path == "/stall" && n == 1:
			w.Write([]byte("par"))
			w.(http.Flusher).Flush()
			<-r.Context().Done() // never finishes
		case r.URL.Path == "/slow":
			// Slower overall than the stall limit, but never silent for long.
			for _, c := range "slow but steady" {
				w.Write([]byte(string(c)))
				w.(http.Flusher).Flush()
				time.Sleep(60 * time.Millisecond)
			}
		default:
			w.Write([]byte("whole file"))
		}
	}))
	defer srv.Close()
	b := &Builder{}
	ctx := context.Background()

	data, err := b.download(ctx, srv.URL+"/stall", 1<<20)
	if err != nil || string(data) != "whole file" || calls.Load() != 2 {
		t.Errorf("stall: %q, %v, %d calls", data, err, calls.Load())
	}
	calls.Store(0)
	if data, err := b.download(ctx, srv.URL+"/slow", 1<<20); err != nil || string(data) != "slow but steady" || calls.Load() != 1 {
		t.Errorf("slow: %q, %v, %d calls", data, err, calls.Load())
	}
	calls.Store(0)
	if _, err := b.download(ctx, srv.URL+"/missing", 1<<20); err == nil || !strings.Contains(err.Error(), "404") || calls.Load() != 1 {
		t.Errorf("missing: %v, %d calls", err, calls.Load())
	}
}
