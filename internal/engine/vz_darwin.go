//go:build darwin

package engine

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/Code-Hex/vz/v3"
)

// VM is a running guest on Apple's Virtualization framework. The framework
// runs the guest in its own sandboxed process, which also serves virtio-fs.
type VM struct {
	vm *vz.VirtualMachine
	// network keeps the VM's end of the frame socket alive while it runs.
	network *os.File
	done    chan struct{}
	once    sync.Once
	err     error
}

// Start boots a VM.
func Start(cfg Config) (*VM, error) {
	boot, err := vz.NewLinuxBootLoader(cfg.Kernel,
		vz.WithCommandLine("console=hvc0 rdinit=/init quiet panic=-1"),
		vz.WithInitrd(cfg.Initramfs),
	)
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	vcfg, err := vz.NewVirtualMachineConfiguration(boot, cfg.CPUs, cfg.MemoryMiB<<20)
	if err != nil {
		return nil, err
	}

	id, err := machineID(cfg.MachineIDFile)
	if err != nil {
		return nil, err
	}
	platform, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(id))
	if err != nil {
		return nil, err
	}
	vcfg.SetPlatformVirtualMachineConfiguration(platform)

	console, err := consoleDevice(cfg.Console)
	if err != nil {
		return nil, err
	}
	vcfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vcfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	vsock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vcfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsock})

	shares := []struct {
		tag, dir string
		readOnly bool
	}{
		{"skerry", cfg.ConfigDir, true},
		{"rootfs", cfg.Rootfs, !cfg.RootfsWritable},
	}
	if cfg.WorkDir != "" {
		shares = append(shares, struct {
			tag, dir string
			readOnly bool
		}{"work", cfg.WorkDir, false})
	}
	var fsDevices []vz.DirectorySharingDeviceConfiguration
	for _, s := range shares {
		dev, err := share(s.tag, s.dir, s.readOnly)
		if err != nil {
			return nil, err
		}
		fsDevices = append(fsDevices, dev)
	}
	vcfg.SetDirectorySharingDevicesVirtualMachineConfiguration(fsDevices)

	if cfg.Network != nil {
		attach, err := vz.NewFileHandleNetworkDeviceAttachment(cfg.Network)
		if err != nil {
			return nil, fmt.Errorf("network: %w", err)
		}
		nic, err := vz.NewVirtioNetworkDeviceConfiguration(attach)
		if err != nil {
			return nil, err
		}
		if cfg.MAC != nil {
			mac, err := vz.NewMACAddress(cfg.MAC)
			if err != nil {
				return nil, err
			}
			nic.SetMACAddress(mac)
		}
		vcfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})
	}
	if cfg.DataDisk != "" {
		// Fsync keeps what the guest flushed safe across a host crash.
		attach, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(cfg.DataDisk, false,
			vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFsync)
		if err != nil {
			return nil, fmt.Errorf("data disk: %w", err)
		}
		disk, err := vz.NewVirtioBlockDeviceConfiguration(attach)
		if err != nil {
			return nil, err
		}
		vcfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{disk})
	}

	if ok, err := vcfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("VM configuration: %w", err)
	}
	vm, err := vz.NewVirtualMachine(vcfg)
	if err != nil {
		return nil, err
	}
	v := &VM{vm: vm, network: cfg.Network, done: make(chan struct{})}
	go v.watch()
	if err := vm.Start(); err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
	}
	return v, nil
}

func (v *VM) watch() {
	for state := range v.vm.StateChangedNotify() {
		switch state {
		case vz.VirtualMachineStateStopped:
			v.finish(nil)
			return
		case vz.VirtualMachineStateError:
			v.finish(errors.New("the VM stopped with an error"))
			return
		}
	}
}

func (v *VM) finish(err error) {
	v.once.Do(func() {
		v.err = err
		if v.network != nil {
			_ = v.network.Close()
		}
		close(v.done)
	})
}

// Done is closed when the VM stops.
func (v *VM) Done() <-chan struct{} { return v.done }

// Err reports why the VM stopped, after Done is closed.
func (v *VM) Err() error { return v.err }

// Dial connects to a vsock port in the guest.
func (v *VM) Dial(port uint32) (net.Conn, error) {
	devices := v.vm.SocketDevices()
	if len(devices) == 0 {
		return nil, errors.New("VM has no vsock device")
	}
	return devices[0].Connect(port)
}

// Stop stops the VM immediately. Prefer asking the agent to power off.
func (v *VM) Stop() error {
	if !v.vm.CanStop() {
		return nil
	}
	return v.vm.Stop()
}

func machineID(path string) (*vz.GenericMachineIdentifier, error) {
	if data, err := os.ReadFile(path); err == nil {
		return vz.NewGenericMachineIdentifierWithData(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	id, err := vz.NewGenericMachineIdentifier()
	if err != nil {
		return nil, err
	}
	return id, os.WriteFile(path, id.DataRepresentation(), 0o600)
}

func consoleDevice(out io.Writer) (*vz.VirtioConsoleDeviceSerialPortConfiguration, error) {
	in, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = io.Discard
	}
	go func() { _, _ = io.Copy(out, r) }()
	attach, err := vz.NewFileHandleSerialPortAttachment(in, w)
	if err != nil {
		return nil, err
	}
	return vz.NewVirtioConsoleDeviceSerialPortConfiguration(attach)
}

func share(tag, dir string, readOnly bool) (vz.DirectorySharingDeviceConfiguration, error) {
	shared, err := vz.NewSharedDirectory(dir, readOnly)
	if err != nil {
		return nil, fmt.Errorf("share %s: %w", dir, err)
	}
	single, err := vz.NewSingleDirectoryShare(shared)
	if err != nil {
		return nil, err
	}
	dev, err := vz.NewVirtioFileSystemDeviceConfiguration(tag)
	if err != nil {
		return nil, err
	}
	dev.SetDirectoryShare(single)
	return dev, nil
}
