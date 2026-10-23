package image

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func fakeImage() []byte {
	img := make([]byte, 4096)
	copy(img[56:], "ARM\x64")
	copy(img[100:], "kernel body")
	return img
}

func zboot(t *testing.T, payload []byte, comp string) []byte {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(payload)
	zw.Close()
	out := make([]byte, 128)
	copy(out, "MZ")
	copy(out[4:], "zimg")
	binary.LittleEndian.PutUint32(out[8:], 128)
	binary.LittleEndian.PutUint32(out[12:], uint32(gz.Len()))
	copy(out[24:], comp)
	return append(out, gz.Bytes()...)
}

func TestUnpackKernel(t *testing.T) {
	img := fakeImage()
	if got, err := UnpackKernel(img); err != nil || !bytes.Equal(got, img) {
		t.Fatalf("plain Image: %v", err)
	}
	if got, err := UnpackKernel(zboot(t, img, "gzip")); err != nil || !bytes.Equal(got, img) {
		t.Fatalf("zboot: %v", err)
	}
	for name, bad := range map[string][]byte{
		"zstd":       zboot(t, img, "zstd"),
		"not kernel": zboot(t, []byte("hello"), "gzip"),
		"short":      []byte("MZ"),
		"random":     bytes.Repeat([]byte{1}, 200),
	} {
		if _, err := UnpackKernel(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	truncated := zboot(t, img, "gzip")
	binary.LittleEndian.PutUint32(truncated[12:], 1<<30)
	if _, err := UnpackKernel(truncated); err == nil {
		t.Error("accepted a payload size past the end of the file")
	}
}

// TestCPIOUnpacks checks the archive with the system's cpio, when present.
func TestCPIOUnpacks(t *testing.T) {
	bin, err := exec.LookPath("cpio")
	if err != nil {
		t.Skip("no cpio on PATH")
	}
	var buf bytes.Buffer
	c := NewCPIO(&buf)
	c.Dir("lib", 0o755)
	c.File("lib/a.ko", 0o644, []byte("module"))
	c.File("init", 0o755, []byte("#!/bin/sh\n"))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(bin, "-idm", "--quiet")
	cmd.Dir = dir
	cmd.Stdin = &buf
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cpio: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "lib/a.ko")); string(got) != "module" {
		t.Errorf("lib/a.ko = %q", got)
	}
	if info, err := os.Stat(filepath.Join(dir, "init")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("init: %v %v", info, err)
	}

	var bad bytes.Buffer
	c = NewCPIO(&bad)
	c.File("../escape", 0o644, nil)
	if err := c.Close(); err == nil {
		t.Error("accepted an unsafe name")
	}
}
