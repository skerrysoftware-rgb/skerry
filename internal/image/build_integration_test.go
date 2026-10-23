package image

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBuildFromAlpine builds a real image from Alpine's mirror. It downloads
// about 70 MB, so it only runs with SKERRY_INTEGRATION=1.
func TestBuildFromAlpine(t *testing.T) {
	if os.Getenv("SKERRY_INTEGRATION") != "1" {
		t.Skip("set SKERRY_INTEGRATION=1 to build from Alpine's mirror")
	}
	keys, err := TrustedKeys()
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("SKERRY_TEST_CACHE")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Builder{Dir: dir, Keys: keys, Arch: "aarch64", Log: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	img, err := b.Build(ctx, Spec{Packages: []string{"nodejs"}, Agent: []byte("#!/bin/sh\n")})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"bin/busybox", "usr/bin/node", "etc/ssl/certs/ca-certificates.crt", ".skerry/agent"} {
		if _, err := os.Lstat(filepath.Join(img.Rootfs, p)); err != nil {
			t.Errorf("rootfs is missing %s: %v", p, err)
		}
	}
	kernel, err := os.ReadFile(img.Kernel)
	if err != nil || !isArm64Image(kernel) {
		t.Errorf("kernel is not an arm64 Image: %v", err)
	}
	if info, err := os.Stat(img.Initramfs); err != nil || info.Size() < 1<<10 {
		t.Errorf("initramfs: %v", err)
	}
	scripts, _ := os.ReadDir(filepath.Join(img.Rootfs, ".skerry", "scripts"))
	t.Logf("image %s: %d install scripts", img.ID[:12], len(scripts))

	// A second build reuses the image.
	again, err := b.Build(ctx, Spec{Packages: []string{"nodejs"}, Agent: []byte("#!/bin/sh\n")})
	if err != nil || again.Dir != img.Dir {
		t.Errorf("rebuild = %s, %v; want reuse of %s", again.Dir, err, img.Dir)
	}
}
