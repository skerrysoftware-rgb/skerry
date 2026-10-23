package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pinnedImage(t *testing.T, b *Builder, id string, finalized bool, packages ...string) Image {
	t.Helper()
	img := b.layout(id)
	if err := os.MkdirAll(img.Rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(img.Dir, manifest{Format: formatVersion, Packages: packages, Kernel: "6.18-r0", Finalized: finalized}); err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPinReusesImageUntilTooOld(t *testing.T) {
	b := &Builder{Dir: t.TempDir(), System: Source{Name: "wolfi"}, Kernel: Source{Name: "alpine"}}
	spec := Spec{Packages: []string{"nodejs-24"}, Agent: []byte("agent")}
	if _, ok := b.Pinned(spec, time.Hour); ok {
		t.Fatal("pinned before anything was built")
	}
	img := pinnedImage(t, b, strings.Repeat("a", 64), true)
	if err := b.Pin(spec, img); err != nil {
		t.Fatal(err)
	}
	got, ok := b.Pinned(spec, time.Hour)
	if !ok || got.ID != img.ID || !got.Finalized {
		t.Fatalf("Pinned = %+v, %v", got, ok)
	}
	// Another spec, or another agent, is another pin.
	if _, ok := b.Pinned(Spec{Packages: []string{"nodejs-24", "valkey"}, Agent: []byte("agent")}, time.Hour); ok {
		t.Error("a different package set shares the pin")
	}
	if _, ok := b.Pinned(Spec{Packages: []string{"nodejs-24"}, Agent: []byte("agent 2")}, time.Hour); ok {
		t.Error("a different agent shares the pin")
	}
	if _, ok := b.Pinned(spec, 0); ok {
		t.Error("an expired pin was used")
	}
}

func TestCollectKeepsPinnedAndInUse(t *testing.T) {
	b := &Builder{Dir: t.TempDir(), System: Source{Name: "wolfi"}, Kernel: Source{Name: "alpine"}}
	pinned := pinnedImage(t, b, strings.Repeat("a", 64), true, "glibc-2.44-r9")
	running := pinnedImage(t, b, strings.Repeat("b", 64), true, "valkey-8.1.2-r0")
	old := pinnedImage(t, b, strings.Repeat("c", 64), true, "nodejs-26-26.12.1-r2")
	fresh := pinnedImage(t, b, strings.Repeat("d", 64), false)
	if err := b.Pin(Spec{Agent: []byte("x")}, pinned); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-2 * time.Hour)
	for _, img := range []Image{pinned, running, old} {
		_ = os.Chtimes(img.Dir, long, long)
	}
	pkgDir := filepath.Join(b.Dir, "packages", "wolfi", "repo", "aarch64")
	_ = os.MkdirAll(pkgDir, 0o755)
	for _, f := range []string{"glibc-2.44-r9.apk", "valkey-8.1.2-r0.apk", "nodejs-26-26.12.1-r2.apk"} {
		_ = os.WriteFile(filepath.Join(pkgDir, f), []byte("x"), 0o644)
	}

	removed, err := b.Collect(map[string]bool{running.ID: true}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range []Image{pinned, running, fresh} {
		if _, err := os.Stat(img.Dir); err != nil {
			t.Errorf("deleted image %s", img.ID[:1])
		}
	}
	if _, err := os.Stat(old.Dir); err == nil {
		t.Error("kept an unused image")
	}
	if _, err := os.Stat(filepath.Join(pkgDir, "nodejs-26-26.12.1-r2.apk")); err == nil {
		t.Error("kept an unused package")
	}
	for _, f := range []string{"glibc-2.44-r9.apk", "valkey-8.1.2-r0.apk"} {
		if _, err := os.Stat(filepath.Join(pkgDir, f)); err != nil {
			t.Errorf("deleted %s, which a kept image uses", f)
		}
	}
	if len(removed) != 2 {
		t.Errorf("removed = %v", removed)
	}
}

func TestConcurrentPins(t *testing.T) {
	b := &Builder{Dir: t.TempDir(), System: Source{Name: "wolfi"}, Kernel: Source{Name: "alpine"}}
	spec := Spec{Agent: []byte("x")}
	img := pinnedImage(t, b, strings.Repeat("e", 64), true)
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		go func() { errs <- b.Pin(spec, img) }()
	}
	for i := 0; i < 50; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := b.Pinned(spec, time.Hour); !ok || got.ID != img.ID {
		t.Errorf("after concurrent pins: %+v, %v", got, ok)
	}
}

func TestCollectDropsExpiredPins(t *testing.T) {
	b := &Builder{Dir: t.TempDir(), System: Source{Name: "wolfi"}, Kernel: Source{Name: "alpine"}}
	img := pinnedImage(t, b, strings.Repeat("f", 64), true)
	if err := b.Pin(Spec{Agent: []byte("old agent")}, img); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(img.Dir, long, long)
	if _, err := b.Collect(nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(img.Dir); err == nil {
		t.Error("an image whose pin expired was kept")
	}
}
