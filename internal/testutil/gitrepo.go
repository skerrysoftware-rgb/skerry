// Package testutil builds throwaway git repositories for tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
)

// IsolateGit points git at an empty global config for the rest of the test,
// so the developer's settings (signing, hooks, templates) never apply.
func IsolateGit(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	content := "[user]\n\tname = skerry test\n\temail = test@example.invalid\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TEMPLATE_DIR", "")
}

// NewRepo creates a repository with one commit and returns its canonical root.
func NewRepo(t *testing.T) string {
	t.Helper()
	IsolateGit(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Workspaces mark their .git pointer immutable; clear that so the
	// temporary directory can be deleted.
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Name() == ".git" {
				_ = fsutil.SetImmutable(p, false)
			}
			return nil
		})
	})
	Git(t, root, "init", "--quiet")
	WriteFile(t, filepath.Join(root, "README.md"), "hello\n")
	Git(t, root, "add", "README.md")
	Git(t, root, "commit", "--quiet", "-m", "initial")
	return root
}

// Git runs git in dir and returns trimmed output, failing the test on error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// WriteFile writes content to path, creating parent directories.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
