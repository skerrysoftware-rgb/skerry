package vmm

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// repoIdentity names a repository by its .git directory's inode and
// creation time, so a different repository cloned later at the same path
// is a different repository. (Not the device number: macOS assigns those
// at mount time.)
func repoIdentity(repo string) (string, error) {
	info, err := os.Lstat(filepath.Join(repo, ".git"))
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("can't identify %s", repo)
	}
	return fmt.Sprintf("%d:%d.%09d", st.Ino, st.Birthtimespec.Sec, st.Birthtimespec.Nsec), nil
}
