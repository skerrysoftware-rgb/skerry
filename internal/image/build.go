package image

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
)

// BasePackages are in every guest system (from the system source, Wolfi).
// e2fsprogs formats each workspace's data disk on first boot.
var BasePackages = []string{"wolfi-baselayout", "busybox", "glibc", "ca-certificates-bundle", "e2fsprogs", "iproute2"}

// BootModules are loaded by the agent at boot, dependencies first: shared
// folders, the overlay, vsock, networking and the data disk.
var BootModules = []string{
	"fuse", "virtiofs", "overlay",
	"vsock", "vmw_vsock_virtio_transport_common", "vmw_vsock_virtio_transport",
	"virtio_net", "virtio_blk", "ext4",
}

// maxImageDownload bounds the packages one image may need.
const maxImageDownload = 3 << 30

// formatVersion changes whenever the image layout changes, so old images
// are rebuilt rather than misread.
const formatVersion = "3"

// Spec describes an image.
type Spec struct {
	// Packages are installed in the rootfs on top of BasePackages.
	Packages []string
	// Agent is the guest agent binary (linux, static).
	Agent []byte
}

// Image is a built image on disk.
type Image struct {
	ID  string
	Dir string
	// Kernel is the uncompressed kernel Image.
	Kernel string
	// Initramfs holds the agent and boot modules.
	Initramfs string
	// Rootfs is shared read-only into each guest.
	Rootfs string
	// Finalized is false until install scripts have run inside a guest.
	Finalized bool
}

type manifest struct {
	Format    string   `json:"format"`
	Packages  []string `json:"packages"`
	Kernel    string   `json:"kernel"`
	Finalized bool     `json:"finalized"`
}

// Builder builds images into a cache directory.
type Builder struct {
	// Dir holds downloads and images.
	Dir string
	// System provides the guest's userland; Kernel provides linux-virt.
	System, Kernel Source
	// Arch is the repositories' architecture name, e.g. "aarch64".
	Arch string
	HTTP *http.Client
	// Log reports progress; nil discards it.
	Log func(format string, args ...any)
}

func (b *Builder) logf(format string, args ...any) {
	if b.Log != nil {
		b.Log(format, args...)
	}
}

// Build returns the image for spec, building it if needed.
func (b *Builder) Build(ctx context.Context, spec Spec) (Image, error) {
	system, err := b.resolver(ctx, b.System)
	if err != nil {
		return Image{}, err
	}
	pkgs, err := system.Resolve(append(append([]string{}, BasePackages...), spec.Packages...))
	if err != nil {
		return Image{}, err
	}
	// A project chooses packages, so bound what it can make skerry fetch.
	var total int64
	for _, p := range pkgs {
		total += p.Size
	}
	if total > maxImageDownload {
		return Image{}, fmt.Errorf("the image would need %d MB of packages, more than the %d MB limit; ask for fewer packages",
			total>>20, int64(maxImageDownload)>>20)
	}
	kernelSrc, err := b.resolver(ctx, b.Kernel)
	if err != nil {
		return Image{}, err
	}
	kernel, err := kernelSrc.Resolve([]string{"linux-virt"})
	if err != nil {
		return Image{}, err
	}
	kpkg := kernel[len(kernel)-1] // the kernel itself; its dependencies are userland tools

	id := imageID(pkgs, kpkg, spec.Agent)
	img := b.layout(id)
	if m, err := readManifest(img.Dir); err == nil && m.Format == formatVersion {
		img.Finalized = m.Finalized
		touch(img.Dir)
		return img, nil
	}

	b.logf("building image %s (%d packages)", id[:12], len(pkgs))
	staging, err := os.MkdirTemp(filepath.Join(b.Dir, "images"), ".build-"+id[:12]+"-")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(staging)

	if err := b.buildRootfs(ctx, filepath.Join(staging, "rootfs"), pkgs, spec.Agent); err != nil {
		return Image{}, err
	}
	if err := b.buildBoot(ctx, staging, kpkg, spec.Agent); err != nil {
		return Image{}, err
	}
	names := make([]string, len(pkgs))
	for i, p := range pkgs {
		names[i] = p.Name + "-" + p.Version
	}
	if err := writeManifest(staging, manifest{Format: formatVersion, Packages: names, Kernel: kpkg.Version}); err != nil {
		return Image{}, err
	}
	if err := os.Rename(staging, img.Dir); err != nil {
		if _, statErr := os.Stat(img.Dir); statErr == nil {
			return img, nil // another process finished the same image first
		}
		return Image{}, err
	}
	b.logf("image %s ready", id[:12])
	return img, nil
}

// MarkFinalized records that install scripts have run.
func (b *Builder) MarkFinalized(img Image) error {
	m, err := readManifest(img.Dir)
	if err != nil {
		return err
	}
	m.Finalized = true
	return writeManifest(img.Dir, m)
}

func (b *Builder) layout(id string) Image {
	dir := filepath.Join(b.Dir, "images", id)
	return Image{
		ID: id, Dir: dir,
		Kernel:    filepath.Join(dir, "Image"),
		Initramfs: filepath.Join(dir, "initramfs.cpio"),
		Rootfs:    filepath.Join(dir, "rootfs"),
	}
}

func imageID(pkgs []Package, kernel Package, agent []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "format %s\nkernel %s %s\n", formatVersion, kernel.Version, kernel.Checksum)
	for _, p := range pkgs {
		fmt.Fprintf(h, "%s %s %s\n", p.Name, p.Version, p.Checksum)
	}
	sum := sha256.Sum256(agent)
	fmt.Fprintf(h, "agent %x\n", sum)
	return hex.EncodeToString(h.Sum(nil))
}

func (b *Builder) buildRootfs(ctx context.Context, dir string, pkgs []Package, agent []byte) error {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(".skerry/scripts", 0o755); err != nil {
		return err
	}
	for i, p := range pkgs {
		v, err := b.fetchPackage(ctx, b.System, p)
		if err != nil {
			return err
		}
		if err := v.Extract(root); err != nil {
			return err
		}
		// Install scripts run later, inside a guest, in this order.
		for _, script := range []string{".pre-install", ".post-install"} {
			if body, ok := v.Scripts[script]; ok {
				name := fmt.Sprintf(".skerry/scripts/%03d-%s%s", i, p.Name, script)
				if err := root.WriteFile(name, body, 0o755); err != nil {
					return err
				}
			}
		}
	}
	if err := busyboxLinks(root); err != nil {
		return err
	}
	return root.WriteFile(".skerry/agent", agent, 0o755)
}

// busyboxLinks creates the command links BusyBox lists in
// etc/busybox-paths.d (sh, ip, mount, ...), as Wolfi's image tool does.
// Existing files win: a real package's command is never replaced.
func busyboxLinks(root *os.Root) error {
	dir, err := root.Open("etc/busybox-paths.d")
	if err != nil {
		return nil // no BusyBox in this image
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		list, err := root.ReadFile("etc/busybox-paths.d/" + name)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(list), "\n") {
			p, err := cleanEntry(strings.TrimSpace(line))
			if err != nil || p == "" || strings.HasSuffix(p, "/busybox") {
				continue
			}
			if _, err := root.Lstat(p); err == nil {
				continue
			}
			if err := makeParent(root, p); err != nil {
				return err
			}
			if err := root.Symlink("/usr/bin/busybox", p); err != nil {
				return fmt.Errorf("busybox link %s: %w", p, err)
			}
		}
	}
	return nil
}

func (b *Builder) buildBoot(ctx context.Context, dir string, kpkg Package, agent []byte) error {
	v, err := b.fetchPackage(ctx, b.Kernel, kpkg)
	if err != nil {
		return err
	}
	kdir := filepath.Join(dir, "kernel-pkg")
	if err := os.Mkdir(kdir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(kdir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := v.Extract(root); err != nil {
		return err
	}

	zimage, err := root.ReadFile("boot/vmlinuz-virt")
	if err != nil {
		return fmt.Errorf("kernel package has no boot/vmlinuz-virt: %w", err)
	}
	kernel, err := UnpackKernel(zimage)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Image"), kernel, 0o644); err != nil {
		return err
	}

	modules, err := kernelModules(root)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	c := NewCPIO(&buf)
	for _, d := range []string{"dev", "proc", "sys", "mnt", "run", "newroot", "lib", "lib/modules"} {
		c.Dir(d, 0o755)
	}
	c.CharDev("dev/console", 0o600, 5, 1)
	c.CharDev("dev/null", 0o666, 1, 3)
	c.File("init", 0o755, agent)
	var order []string
	for _, m := range modules {
		c.File("lib/modules/"+m.name+".ko", 0o644, m.data)
		order = append(order, m.name)
	}
	c.File("lib/modules/load-order", 0o644, []byte(strings.Join(order, "\n")+"\n"))
	if err := c.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "initramfs.cpio"), buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.RemoveAll(kdir)
}

type module struct {
	name string
	data []byte
}

// kernelModules returns BootModules and their dependencies, decompressed,
// in load order.
func kernelModules(root *os.Root) ([]module, error) {
	entries, err := fsReadDir(root, "lib/modules")
	if err != nil || len(entries) != 1 {
		return nil, fmt.Errorf("kernel package should hold one lib/modules/<version>: %v", err)
	}
	base := "lib/modules/" + entries[0]
	depFile, err := root.ReadFile(base + "/modules.dep")
	if err != nil {
		return nil, err
	}
	paths := map[string]string{} // module name -> path
	deps := map[string][]string{}
	sc := bufio.NewScanner(bytes.NewReader(depFile))
	for sc.Scan() {
		target, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name := moduleName(target)
		paths[name] = target
		for _, d := range strings.Fields(rest) {
			deps[name] = append(deps[name], moduleName(d))
		}
	}

	var (
		order []module
		done  = map[string]bool{}
		visit func(string) error
	)
	visit = func(name string) error {
		if done[name] {
			return nil
		}
		done[name] = true
		for _, d := range deps[name] {
			if err := visit(d); err != nil {
				return err
			}
		}
		p, ok := paths[name]
		if !ok {
			return fmt.Errorf("kernel module %s not found", name)
		}
		data, err := root.ReadFile(base + "/" + p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".gz") {
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return fmt.Errorf("module %s: %w", name, err)
			}
			if data, err = io.ReadAll(io.LimitReader(zr, 64<<20)); err != nil {
				return fmt.Errorf("module %s: %w", name, err)
			}
		}
		order = append(order, module{name: name, data: data})
		return nil
	}
	for _, m := range BootModules {
		if err := visit(m); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func moduleName(p string) string {
	base := path.Base(p)
	for _, ext := range []string{".gz", ".xz", ".zst"} {
		base = strings.TrimSuffix(base, ext)
	}
	return strings.TrimSuffix(base, ".ko")
}

func fsReadDir(root *os.Root, dir string) ([]string, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	sort.Strings(names)
	return names, err
}

// resolver loads and verifies every repository index of a source.
func (b *Builder) resolver(ctx context.Context, src Source) (*Resolver, error) {
	var all []Package
	for _, repo := range src.Repos {
		text, err := b.index(ctx, src, repo)
		if err != nil {
			return nil, err
		}
		pkgs, err := ParseIndex(text, repo)
		if err != nil {
			return nil, fmt.Errorf("%s index: %w", repo, err)
		}
		all = append(all, pkgs...)
	}
	return NewResolver(all), nil
}

// indexMaxAge is how long a downloaded index is reused before refreshing.
const indexMaxAge = 24 * time.Hour

func (b *Builder) index(ctx context.Context, src Source, repo string) (string, error) {
	cached := filepath.Join(b.repoDir(src, repo), "APKINDEX.tar.gz")
	if info, err := os.Stat(cached); err == nil && time.Since(info.ModTime()) < indexMaxAge {
		data, err := os.ReadFile(cached)
		if err == nil {
			if text, err := VerifyIndex(src.Keys, data); err == nil {
				return text, nil
			}
		}
	}
	data, err := b.download(ctx, repo+"/"+b.Arch+"/APKINDEX.tar.gz", 64<<20)
	if err != nil {
		// Offline: an older index that still verifies is better than not
		// starting at all. Fresh packages wait for the next refresh.
		if cachedData, cerr := os.ReadFile(cached); cerr == nil {
			if text, verr := VerifyIndex(src.Keys, cachedData); verr == nil {
				b.logf("using the cached %s index (refresh failed: %v)", repo, err)
				return text, nil
			}
		}
		return "", err
	}
	text, err := VerifyIndex(src.Keys, data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", repo, err)
	}
	if err := os.MkdirAll(b.repoDir(src, repo), 0o755); err != nil {
		return "", err
	}
	return text, fsutil.WriteFileAtomic(cached, data, 0o644)
}

func (b *Builder) fetchPackage(ctx context.Context, src Source, p Package) (*Verified, error) {
	cached := filepath.Join(b.repoDir(src, p.Repo), p.File())
	if data, err := os.ReadFile(cached); err == nil {
		if v, err := VerifyPackage(src.Keys, p, data); err == nil {
			return v, nil
		}
	}
	b.logf("downloading %s", p.File())
	data, err := b.download(ctx, p.Repo+"/"+b.Arch+"/"+p.File(), p.Size)
	if err != nil {
		return nil, err
	}
	v, err := VerifyPackage(src.Keys, p, data)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(b.repoDir(src, p.Repo), 0o755); err != nil {
		return nil, err
	}
	return v, fsutil.WriteFileAtomic(cached, data, 0o644)
}

func (b *Builder) repoDir(src Source, repo string) string {
	return filepath.Join(b.Dir, "packages", src.Name, repoKey(repo), b.Arch)
}

// Downloads give up on a connection only when it stalls, not because a
// large package on a slow link takes a while, and try again on a new
// connection (often a different server of the mirror). What arrives is
// checked against the signed index either way.
var (
	downloadStall    = 30 * time.Second
	downloadAttempts = 3
)

func (b *Builder) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		var data []byte
		var retry bool
		if data, retry, err = b.downloadOnce(ctx, url, limit); err == nil || !retry || ctx.Err() != nil {
			return data, err
		}
		if attempt < downloadAttempts {
			b.logf("%v; trying again", err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return nil, err
}

// downloadOnce fetches url once; retry says whether another attempt could
// succeed (a network failure or a server error, not a missing file).
func (b *Builder) downloadOnce(ctx context.Context, url string, limit int64) (data []byte, retry bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stalled := time.AfterFunc(downloadStall, cancel)
	defer stalled.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	client := b.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, true, stallError(url, err, stalled)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err = io.ReadAll(io.LimitReader(progressReader{resp.Body, func() { stalled.Reset(downloadStall) }}, limit+1))
	if err != nil {
		return nil, true, stallError(url, err, stalled)
	}
	if int64(len(data)) > limit {
		return nil, false, fmt.Errorf("GET %s: larger than expected", url)
	}
	return data, false, nil
}

// stallError says a download stalled when that is what ended it.
func stallError(url string, err error, stalled *time.Timer) error {
	if !stalled.Stop() { // it fired: the connection made no progress
		return fmt.Errorf("GET %s: no data for %s", url, downloadStall)
	}
	return fmt.Errorf("GET %s: %w", url, err)
}

// progressReader calls progress whenever data arrives.
type progressReader struct {
	r        io.Reader
	progress func()
}

func (p progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.progress()
	}
	return n, err
}

func readManifest(dir string) (manifest, error) {
	var m manifest
	data, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if m.Format == "" {
		return m, errors.New("image manifest has no format")
	}
	return m, nil
}

func writeManifest(dir string, m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, "image.json"), append(data, '\n'), 0o644)
}
