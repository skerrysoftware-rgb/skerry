package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// maxPackageFile bounds a single file extracted from a package.
const maxPackageFile = 1 << 30

// Extract writes the package's files into root. Every write goes through
// os.Root, so neither a crafted path nor a symlink planted by an earlier
// entry can reach outside the image directory.
func (v *Verified) Extract(root *os.Root) error {
	zr, err := gzip.NewReader(bytes.NewReader(v.data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	name := v.Package.Name
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		p, err := cleanEntry(hdr.Name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if p == "" {
			continue
		}
		mode := fs.FileMode(hdr.Mode) & 0o777 // no setuid, setgid or sticky bits
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(p, 0o755); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := root.Chmod(p, mode|0o700); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		case tar.TypeReg:
			if hdr.Size > maxPackageFile {
				return fmt.Errorf("%s: %s is too large", name, p)
			}
			if err := makeParent(root, p); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			// Replace, never write through, whatever is there: an existing
			// symlink must not redirect this write.
			if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s: %w", name, err)
			}
			f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o600)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			_, err = io.Copy(f, io.LimitReader(tr, hdr.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("%s: %s: %w", name, p, err)
			}
		case tar.TypeSymlink:
			// The target is stored as-is: it is interpreted inside the guest,
			// and os.Root refuses to follow it outside the image on the host.
			if err := makeParent(root, p); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := root.Symlink(hdr.Linkname, p); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		case tar.TypeLink:
			target, err := cleanEntry(hdr.Linkname)
			if err != nil || target == "" {
				return fmt.Errorf("%s: bad hard link %q", name, hdr.Linkname)
			}
			if err := makeParent(root, p); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := root.Link(target, p); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		default:
			// Device nodes, FIFOs and metadata entries are not image content.
		}
	}
}

// cleanEntry turns a tar entry name into a relative path inside the image,
// refusing anything that climbs out of it.
func cleanEntry(name string) (string, error) {
	if strings.ContainsRune(name, 0) || strings.Contains(name, "\\") {
		return "", fmt.Errorf("unsafe entry name %q", name)
	}
	p := path.Clean("/" + name)[1:]
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("unsafe entry name %q", name)
		}
	}
	return p, nil
}

func makeParent(root *os.Root, p string) error {
	if dir := path.Dir(p); dir != "." {
		return root.MkdirAll(dir, 0o755)
	}
	return nil
}
