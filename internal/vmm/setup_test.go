package vmm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/registry"
)

func TestOpenDumpRefusesTheCheckout(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "repo")
	ws := registry.Workspace{Repo: repo, Path: filepath.Join(repo, ".claude", "worktrees", "w")}
	secret := filepath.Join(base, "id_ed25519")
	outside := filepath.Join(base, "dumps", "prod.sql")
	for _, d := range []string{ws.Path, filepath.Dir(outside)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600)
	_ = os.WriteFile(outside, []byte("select 1;"), 0o600)
	// A link planted in the checkout (by the VM or the repository).
	planted := filepath.Join(ws.Path, "fixtures.sql")
	if err := os.Symlink(secret, planted); err != nil {
		t.Fatal(err)
	}
	inRepo := filepath.Join(repo, "seed.sql")
	_ = os.WriteFile(inRepo, []byte("select 1;"), 0o600)
	for _, f := range []string{planted, inRepo} {
		if file, err := openDump(ws, f); err == nil {
			file.Close()
			t.Errorf("opened %s", f)
		}
	}
	// A link elsewhere pointing into the checkout is refused too.
	link := filepath.Join(base, "dumps", "link.sql")
	_ = os.Symlink(inRepo, link)
	if file, err := openDump(ws, link); err == nil {
		file.Close()
		t.Error("followed a link into the repository")
	}
	fifo := filepath.Join(base, "dumps", "fifo.sql")
	if err := syscallMkfifo(fifo); err == nil {
		if file, err := openDump(ws, fifo); err == nil {
			file.Close()
			t.Error("opened a FIFO")
		}
	}
	file, err := openDump(ws, outside)
	if err != nil {
		t.Fatalf("a dump outside the checkout: %v", err)
	}
	file.Close()
}

// The seed scripts run in the guest's busybox sh; check them with the
// host's sh against a fake psql.
func TestSeedScriptDecides(t *testing.T) {
	bin := t.TempDir()
	for tables, want := range map[string]string{"0": "loading", "3": "already has tables"} {
		fake := "#!/bin/sh\ncase \"$*\" in *count*) echo " + tables + " ;; *) cat >/dev/null ;; esac\n"
		if err := os.WriteFile(filepath.Join(bin, "psql"), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", seedScript)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "SKERRY_SEED=seed.sql"}
		cmd.Dir = t.TempDir()
		_ = os.WriteFile(filepath.Join(cmd.Dir, "seed.sql"), []byte("select 1;"), 0o644)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Errorf("with %s tables: %q, %v; want %q", tables, out, err, want)
		}
	}
}

func TestOpenDumpRefusesLinkChainThroughTheRepository(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "repo")
	ws := registry.Workspace{Repo: repo, Path: filepath.Join(repo, ".claude", "worktrees", "w")}
	_ = os.MkdirAll(ws.Path, 0o755)
	secret := filepath.Join(base, "id_ed25519")
	_ = os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600)
	// The user's own link points into the repository, where the
	// repository has replaced the file with a link to the secret.
	if err := os.Symlink(secret, filepath.Join(repo, "seed.sql")); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(base, "mydump.sql")
	if err := os.Symlink(filepath.Join(repo, "seed.sql"), mine); err != nil {
		t.Fatal(err)
	}
	if f, err := openDump(ws, mine); err == nil {
		f.Close()
		t.Fatal("followed a link chain through the repository")
	}
}

func TestSeedScriptsUseOneTransaction(t *testing.T) {
	if !strings.Contains(seedScript, loadSQL+" -f -") {
		t.Error("the gzip seed reads standard input without -f -, so it isn't one transaction")
	}
}
