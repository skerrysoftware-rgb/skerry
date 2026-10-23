// Package agent is skerry's guest agent and the host's client for it.
//
// The agent runs as PID 1 in each workspace VM. It mounts the shared
// filesystems, then serves SSH over vsock so the host can run commands. The
// host generates both SSH keys per workspace and pins them, so neither side
// trusts on first use. The guest is treated as hostile: the host never acts
// on anything it reports beyond command output and exit status.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/skerrysoftware-rgb/skerry/internal/services"
)

// Port is the vsock port the agent's SSH server listens on.
const Port = 22

// Virtio-fs tags the host shares into the guest.
const (
	TagConfig = "skerry"
	TagRootfs = "rootfs"
	TagWork   = "work"
)

// Files in the config share.
const (
	ConfigFile        = "agent.json"
	HostKeyFile       = "host_key"
	AuthorizedKeyFile = "authorized_key"
)

// Modes the agent runs in.
const (
	// ModeServe runs a workspace: overlay rootfs, SSH server.
	ModeServe = "serve"
	// ModeFinalize mounts the image rootfs writable, runs the packages'
	// install scripts once, then powers off.
	ModeFinalize = "finalize"
)

// Config is what the host tells the agent at boot, via the config share.
type Config struct {
	Mode string `json:"mode"`
	// WorkspacePath is the checkout's path on the host. The agent mounts the
	// checkout at the same path, so paths mean the same thing on both sides.
	WorkspacePath string `json:"workspace_path,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	// Network configures the VM's network card; nil leaves it down.
	Network *Network `json:"network,omitempty"`
	// DataDisk says the VM has a persistent disk for service data. The
	// agent formats it on first boot if, and only if, it is blank.
	DataDisk bool `json:"data_disk,omitempty"`
	// Services are preset names the agent runs and supervises.
	Services []string `json:"services,omitempty"`
	// Secret is the workspace's password for its services.
	Secret string `json:"secret,omitempty"`
}

// Network is the guest side of the workspace network.
type Network struct {
	Address netip.Prefix `json:"address"`
	Gateway netip.Addr   `json:"gateway"`
	DNS     netip.Addr   `json:"dns"`
}

var secretPattern = regexp.MustCompile(`^[0-9a-f]{32,64}$`)

var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Validate checks a config. The host writes it, but the agent still refuses
// anything that could mount outside sensible places.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeServe:
		if c.WorkspacePath == "" || !filepath.IsAbs(c.WorkspacePath) || filepath.Clean(c.WorkspacePath) != c.WorkspacePath {
			return fmt.Errorf("workspace path %q must be absolute and clean", c.WorkspacePath)
		}
		for _, reserved := range []string{"/", "/proc", "/sys", "/dev", "/run", "/tmp", "/bin", "/sbin", "/lib", "/usr", "/etc", "/.skerry"} {
			if c.WorkspacePath == reserved || isWithin(c.WorkspacePath, reserved) && reserved != "/" {
				return fmt.Errorf("workspace path %q overlaps the guest's %s", c.WorkspacePath, reserved)
			}
		}
	case ModeFinalize:
	default:
		return fmt.Errorf("unknown mode %q", c.Mode)
	}
	if c.Hostname != "" && !hostnamePattern.MatchString(c.Hostname) {
		return fmt.Errorf("invalid hostname %q", c.Hostname)
	}
	if n := c.Network; n != nil {
		if !n.Address.IsValid() || !n.Address.Addr().Is4() || !n.Gateway.Is4() || !n.DNS.Is4() || !n.Address.Contains(n.Gateway) {
			return fmt.Errorf("invalid network %+v", *n)
		}
	}
	if len(c.Services) > 0 {
		norm, err := services.Normalize(c.Services)
		if err != nil {
			return err
		}
		if !slices.Equal(norm, c.Services) {
			return fmt.Errorf("services %v are not normalized", c.Services)
		}
		if !c.DataDisk {
			return errors.New("services need a data disk")
		}
		if !secretPattern.MatchString(c.Secret) {
			return errors.New("services need a secret of 32 to 64 hex digits")
		}
	}
	return nil
}

func isWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && (len(rel) < 3 || rel[:3] != "../")
}

// WriteConfig writes cfg into the config share directory.
func WriteConfig(dir string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ConfigFile), data, 0o600)
}

// ReadConfig reads and validates the config from the config share.
func ReadConfig(dir string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return cfg, err
	}
	if len(data) > 64<<10 {
		return cfg, errors.New("agent config is too large")
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("agent config: %w", err)
	}
	return cfg, cfg.Validate()
}
