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

// Guest paths for networking and service data.
const (
	dataDev   = "/dev/vda"
	dataMount = "/var/lib/skerry"
)

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

// mountDataDisk mounts the workspace's persistent disk at dataMount. A
// blank disk is formatted first; a disk holding anything but ext4 is left
// alone rather than risk destroying data.
func mountDataDisk() error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(dataDev); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", dataDev)
		}
		time.Sleep(50 * time.Millisecond)
	}
	kind, err := diskKind(dataDev)
	if err != nil {
		return err
	}
	switch kind {
	case "blank":
		logger.Printf("formatting the data disk")
		out, err := command("mkfs.ext4", "-q", "-F", "-L", "skerry-data", "-m", "0", dataDev).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mkfs.ext4: %v: %s", err, bytes.TrimSpace(out))
		}
	case "ext4":
	default:
		return fmt.Errorf("%s holds something other than ext4; not touching it", dataDev)
	}
	if err := os.MkdirAll(dataMount, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(dataDev, dataMount, "ext4", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOATIME, ""); err != nil {
		return fmt.Errorf("mount data disk: %w", err)
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
