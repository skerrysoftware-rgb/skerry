//go:build linux

package agent

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// touchAll sets each path's times to what they already are. Nothing
// changes, but the guest kernel raises an inotify event (IN_ATTRIB, which
// needs both times set), which is what file watchers in the VM need to see
// an edit made on the host. The times are read fresh from the host, not
// from the guest's cache, so a stale cached time can't move a host edit
// into the past. A path that no longer exists has its directory touched
// instead.
func touchAll(workspace string, rels []string) {
	seen := map[string]bool{}
	for _, rel := range rels {
		if rel == "" || !filepath.IsLocal(rel) {
			continue
		}
		path := filepath.Join(workspace, rel)
		for !seen[path] {
			seen[path] = true
			var st unix.Statx_t
			err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_FORCE_SYNC,
				unix.STATX_ATIME|unix.STATX_MTIME, &st)
			if err != nil {
				if path == workspace {
					break
				}
				path = filepath.Dir(path) // deleted: its directory changed
				continue
			}
			// (A host write landing between these two calls would get its
			// old times back; the window is a few microseconds.)
			_ = unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{
				{Sec: st.Atime.Sec, Nsec: int64(st.Atime.Nsec)},
				{Sec: st.Mtime.Sec, Nsec: int64(st.Mtime.Nsec)},
			}, unix.AT_SYMLINK_NOFOLLOW)
			break
		}
	}
}
