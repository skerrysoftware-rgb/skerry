//go:build linux

package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Guest paths for service data and the home disk. The data disk, when
// there is one, is the first block device; the home disk follows it.
const (
	dataMount = "/var/lib/skerry"
	homeMount = "/var/cache/skerry-home"
)

// diskDevices are the block devices in the order the host attaches disks.
var diskDevices = []string{"/dev/vda", "/dev/vdb"}

// setupNetwork brings up the VM's network card with the address the host
// assigned and points DNS at the gateway, which answers through the host.
func setupNetwork(n *Network) error {
	iface, err := firstEthernet()
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		{"link", "set", iface, "up"},
		{"addr", "add", n.Address.String(), "dev", iface},
		{"route", "add", "default", "via", n.Gateway.String(), "dev", iface},
	} {
		if out, err := command("ip", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("ip %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
		}
	}
	// /etc is in the in-memory overlay, so this never reaches the image.
	return os.WriteFile("/etc/resolv.conf", []byte("nameserver "+n.DNS.String()+"\n"), 0o644)
}

// firstEthernet waits for the network card's driver and returns its name.
func firstEthernet() (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir("/sys/class/net")
		for _, e := range entries {
			if e.Name() != "lo" {
				return e.Name(), nil
			}
		}
		if time.Now().After(deadline) {
			return "", errors.New("no network card appeared")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// command runs a tool from the image with the agent's fixed PATH.
// PID 1 starts with no PATH of its own, so the tool is found here rather
// than by exec.Command.
func command(name string, args ...string) *exec.Cmd {
	path, err := lookPath(name)
	if err != nil {
		path = name // Start reports it missing
	}
	cmd := exec.Command(path, args...)
	cmd.Env = []string{"PATH=" + baseEnv["PATH"], "HOME=/root", "LANG=C.UTF-8"}
	return cmd
}

func lookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(baseEnv["PATH"]) {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// mountDisk mounts a workspace disk at mount. A blank disk is formatted
// first; a disk holding anything but ext4, or ext4 labelled for another
// use (the data disk where the home disk should be), is left alone rather
// than risk destroying data. The file system is checked first, as an
// unclean stop can leave it needing repair. A disposable disk (caches)
// that can't be repaired or mounted is formatted again.
func mountDisk(dev, mount, label string, disposable bool) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(dev); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", dev)
		}
		time.Sleep(50 * time.Millisecond)
	}
	format := func() error {
		logger.Printf("formatting %s", label)
		out, err := command("mkfs.ext4", "-q", "-F", "-L", label, "-m", "0", dev).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mkfs.ext4: %v: %s", err, bytes.TrimSpace(out))
		}
		return nil
	}
	kind, err := diskKind(dev)
	if err != nil {
		return err
	}
	switch kind {
	case "blank":
		if err := format(); err != nil {
			return err
		}
	case "ext4":
		if got, err := diskLabel(dev); err != nil || got != label {
			return fmt.Errorf("%s is labelled %q, not %s; not touching it", dev, got, label)
		}
		// -p repairs what is safe to repair unattended; 4 and up mean it
		// couldn't.
		if out, err := command("e2fsck", "-p", dev).CombinedOutput(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() >= 4 {
				logger.Printf("e2fsck %s: %v: %s", label, err, bytes.TrimSpace(out))
				if disposable {
					if err := format(); err != nil {
						return err
					}
				}
			}
		}
	default:
		return fmt.Errorf("%s holds something other than ext4; not touching it", dev)
	}
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return err
	}
	// discard hands blocks the guest frees back to the host's sparse file.
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOATIME)
	err = unix.Mount(dev, mount, "ext4", flags, "discard")
	if err != nil && disposable {
		logger.Printf("mount %s: %v; formatting it again", label, err)
		if err := format(); err != nil {
			return err
		}
		err = unix.Mount(dev, mount, "ext4", flags, "discard")
	}
	if err != nil {
		return fmt.Errorf("mount %s: %w", label, err)
	}
	return nil
}

// diskLabel reads an ext4 file system's volume name.
func diskLabel(dev string) (string, error) {
	f, err := os.Open(dev)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// The superblock starts at 1024; the 16-byte volume name is at 120.
	buf := make([]byte, 16)
	if _, err := f.ReadAt(buf, 1024+120); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf, "\x00")), nil
}

// mountHome puts /root and /tmp on the home disk (at dev), so caches,
// downloads and temporary files use the disk, not the memory the root
// filesystem lives in. /root keeps what it held across boots (warm
// caches); /tmp starts empty each boot, as a tmpfs would.
func mountHome(dev string) error {
	if err := mountDisk(dev, homeMount, "skerry-home", true); err != nil {
		return err
	}
	root, tmp := filepath.Join(homeMount, "root"), filepath.Join(homeMount, "tmp")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	// Last boot's /tmp goes (and any an earlier boot didn't finish
	// removing); renamed first, so a big one doesn't hold up the boot, and
	// removed in the background.
	stale, _ := filepath.Glob(filepath.Join(homeMount, "tmp.old.*"))
	if _, err := os.Lstat(tmp); err == nil {
		old := filepath.Join(homeMount, fmt.Sprintf("tmp.old.%d", time.Now().UnixNano()))
		if err := os.Rename(tmp, old); err == nil {
			stale = append(stale, old)
		}
	}
	go func() {
		for _, s := range stale {
			_ = os.RemoveAll(s)
		}
	}()
	if err := os.Mkdir(tmp, 0o1777); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o1777|os.ModeSticky); err != nil {
		return err
	}
	for _, b := range []struct{ from, to string }{{root, "/root"}, {tmp, "/tmp"}} {
		if err := os.MkdirAll(b.to, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(b.from, b.to, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s: %w", b.to, err)
		}
		// The same flags as the disk's mount, on the bind mount itself.
		if err := unix.Mount("", b.to, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOATIME, ""); err != nil {
			logger.Printf("remount %s: %v", b.to, err)
		}
	}
	return nil
}

// diskKind reports "ext4" if dev has an ext2/3/4 superblock, "blank" if its
// first megabyte is all zeros, and "unknown" otherwise.
func diskKind(dev string) (string, error) {
	f, err := os.Open(dev)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	n, err := f.ReadAt(buf, 0)
	if err != nil && n < 2048 {
		return "", fmt.Errorf("read %s: %w", dev, err)
	}
	buf = buf[:n]
	// The superblock starts at 1024; its magic number 0xEF53 is at 56.
	if buf[1024+56] == 0x53 && buf[1024+57] == 0xEF {
		return "ext4", nil
	}
	for _, b := range buf {
		if b != 0 {
			return "unknown", nil
		}
	}
	return "blank", nil
}

func isMountPoint(dir string) bool {
	var st, parent unix.Stat_t
	if unix.Stat(dir, &st) != nil || unix.Stat(filepath.Dir(dir), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// packageManagerShims makes yarn and pnpm commands of their own, run
// through corepack, so `yarn dev` works as it does on a developer's
// machine with corepack enabled. /usr/local/bin is in the in-memory
// overlay, so this happens at every boot and never reaches the image.
func packageManagerShims() {
	corepack, err := lookPath("corepack")
	if err != nil {
		return
	}
	if err := os.MkdirAll("/usr/local/bin", 0o755); err != nil {
		return
	}
	for _, name := range []string{"yarn", "yarnpkg", "pnpm", "pnpx"} {
		if _, err := lookPath(name); err == nil {
			continue // the image has its own
		}
		tool := name
		switch name {
		case "yarnpkg":
			tool = "yarn"
		case "pnpx":
			tool = "pnpm dlx"
		}
		script := "#!/bin/sh\nexec " + corepack + " " + tool + ` "$@"` + "\n"
		_ = os.WriteFile(filepath.Join("/usr/local/bin", name), []byte(script), 0o755)
	}
}
