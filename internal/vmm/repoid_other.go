//go:build !darwin

package vmm

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// repoIdentity names a repository by its .git directory's inode, so a
// different repository cloned later at the same path is (very likely) a
// different repository.
func repoIdentity(repo string) (string, error) {
	info, err := os.Lstat(filepath.Join(repo, ".git"))
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("can't identify %s", repo)
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}
