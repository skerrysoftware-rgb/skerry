package scope_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/scope"
)

func TestEnableCoversSubfolders(t *testing.T) {
	dir := t.TempDir()
	s := scope.Open(dir)

	if ok, err := s.IsEnabled("/work/app"); err != nil || ok {
		t.Fatalf("fresh store enables /work/app: %v, %v", ok, err)
	}
	for range 2 { // enabling twice is harmless
		if err := s.Enable("/work"); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]bool{
		"/work":          true,
		"/work/app":      true,
		"/work/app/deep": true,
		"/workshop":      false,
		"/other":         false,
	} {
		if got, _ := s.IsEnabled(path); got != want {
			t.Errorf("IsEnabled(%s) = %v, want %v", path, got, want)
		}
	}
	if c, _ := s.Covering("/work/app"); c != "/work" {
		t.Errorf("Covering = %q", c)
	}
	if entries, _ := s.List(); len(entries) != 1 {
		t.Fatalf("List = %+v, want one entry", entries)
	}

	if removed, err := s.Disable("/work/app"); err != nil || removed {
		t.Fatalf("Disable of a covered subfolder = %v, %v; want not removed", removed, err)
	}
	if removed, err := s.Disable("/work"); err != nil || !removed {
		t.Fatalf("Disable = %v, %v", removed, err)
	}
	if ok, _ := s.IsEnabled("/work/app"); ok {
		t.Fatal("still enabled after Disable")
	}

	info, err := os.Stat(filepath.Join(dir, "scope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("scope file mode = %o, want 600", perm)
	}
}

func TestFirstHint(t *testing.T) {
	s := scope.Open(t.TempDir())
	if first, err := s.FirstHint("/r"); err != nil || !first {
		t.Fatalf("first hint = %v, %v", first, err)
	}
	if first, _ := s.FirstHint("/r"); first {
		t.Fatal("second hint reported as first")
	}
	if first, _ := s.FirstHint("/r2"); !first {
		t.Fatal("another repository's first hint not reported")
	}
}
