package project

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The checkout is writable from the VM, which could swap a config file for
// a FIFO; reading it must fail, not hang.
func TestFIFODoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, FileName), 0o644); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Load(dir); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was accepted as skerry.yaml")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}
