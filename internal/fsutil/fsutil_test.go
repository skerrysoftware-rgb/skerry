package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCappedLogNeverExceedsCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	l, err := OpenCappedLog(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 99)
	line = append(line, '\n')
	for range 1000 { // 100 KB through a 1 KB cap
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Write(bytes.Repeat([]byte("y"), 5000)); err != nil { // one oversized write
		t.Fatal(err)
	}
	l.Close()
	for _, p := range []string{path, path + ".1"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 1000 {
			t.Errorf("%s is %d bytes, over the 1000-byte cap", filepath.Base(p), info.Size())
		}
	}
}

func TestTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("first TryLock = %v, %v", ok, err)
	}
	if _, ok, err := TryLock(path); err != nil || ok {
		t.Fatalf("second TryLock while held = %v, %v; want false", ok, err)
	}
	unlock()
	unlock2, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("TryLock after release = %v, %v", ok, err)
	}
	unlock2()
}
