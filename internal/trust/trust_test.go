package trust_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/trust"
)

func TestTrustLifecycle(t *testing.T) {
	dir := t.TempDir()
	s := trust.Open(dir)

	if ok, err := s.IsTrusted("/repo"); err != nil || ok {
		t.Fatalf("fresh store trusts /repo: %v, %v", ok, err)
	}
	for range 2 { // trusting twice is harmless
		if err := s.Trust("/repo"); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := s.IsTrusted("/repo"); !ok {
		t.Fatal("trusted repo not reported as trusted")
	}
	if ok, _ := s.IsTrusted("/repo/sub"); ok {
		t.Fatal("trust leaked to a subdirectory path")
	}
	if entries, _ := s.List(); len(entries) != 1 {
		t.Fatalf("List = %+v, want one entry", entries)
	}

	if removed, err := s.Untrust("/repo"); err != nil || !removed {
		t.Fatalf("Untrust = %v, %v", removed, err)
	}
	if ok, _ := s.IsTrusted("/repo"); ok {
		t.Fatal("untrusted repo still trusted")
	}
	if removed, _ := s.Untrust("/repo"); removed {
		t.Fatal("second Untrust reported a removal")
	}

	info, err := os.Stat(filepath.Join(dir, "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("trust store mode = %o, want 600", perm)
	}
}
