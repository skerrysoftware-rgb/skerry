package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
)

// Wolfi is a rolling distribution: some package in an image's closure
// changes almost every day. Rather than rebuild each time, a spec (the
// packages asked for, and the agent) is pinned to the image last built for
// it, which is reused until the pin is older than a maximum age.

type pin struct {
	ID    string    `json:"id"`
	Built time.Time `json:"built"`
}

func (b *Builder) pinPath(spec Spec) string {
	h := sha256.New()
	fmt.Fprintf(h, "format %s\nsystem %s\nkernel %s\n", formatVersion, b.System.Name, b.Kernel.Name)
	names := append(append([]string{}, BasePackages...), spec.Packages...)
	slices.Sort(names)
	fmt.Fprintf(h, "packages %s\n", strings.Join(names, " "))
	agent := sha256.Sum256(spec.Agent)
	fmt.Fprintf(h, "agent %x\n", agent)
	return filepath.Join(b.Dir, "images", "pins", hex.EncodeToString(h.Sum(nil))+".json")
}

// Pinned returns the image pinned for spec if the pin is younger than
// maxAge and the image is still there.
func (b *Builder) Pinned(spec Spec, maxAge time.Duration) (Image, bool) {
	data, err := os.ReadFile(b.pinPath(spec))
	if err != nil {
		return Image{}, false
	}
	var p pin
	if json.Unmarshal(data, &p) != nil || !validID(p.ID) || time.Since(p.Built) > maxAge {
		return Image{}, false
	}
	img := b.layout(p.ID)
	m, err := readManifest(img.Dir)
	if err != nil || m.Format != formatVersion {
		return Image{}, false
	}
	img.Finalized = m.Finalized
	touch(img.Dir)
	return img, true
}

// touch marks an image as just used, so skerry gc (which spares images
// changed in the last hour) can't delete it while a workspace starts on it.
func touch(dir string) {
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
}

// Pin records img, just built or checked against the newest packages, as
// the image for spec from now on. Concurrent pins of the same spec are
// safe: each write is atomic, and the last one wins.
func (b *Builder) Pin(spec Spec, img Image) error {
	path := b.pinPath(spec)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	touch(img.Dir)
	data, err := json.Marshal(pin{ID: img.ID, Built: time.Now().UTC()})
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data, 0o644)
}

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// Collect deletes images that no current pin and no inUse entry names,
// pins that have expired, builds abandoned more than an hour ago, and
// cached packages no remaining image uses. Images pinned longer ago than
// maxAge would be rebuilt at the next start anyway, so they go too. It
// returns what it deleted.
func (b *Builder) Collect(inUse map[string]bool, maxAge time.Duration) ([]string, error) {
	imagesDir := filepath.Join(b.Dir, "images")
	var removed []string
	keep := map[string]bool{}
	for id := range inUse {
		keep[id] = true
	}
	pins, _ := filepath.Glob(filepath.Join(imagesDir, "pins", "*"))
	for _, path := range pins {
		var p pin
		data, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(path, ".json") && json.Unmarshal(data, &p) == nil &&
			validID(p.ID) && time.Since(p.Built) <= maxAge {
			keep[p.ID] = true
			continue
		}
		if info, err := os.Lstat(path); err == nil && time.Since(info.ModTime()) > time.Hour {
			_ = os.Remove(path) // expired, unreadable, or a stray temporary file
		}
	}
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	keepFiles := map[string]bool{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".build-") {
			// A build that crashed; one in progress is younger than this.
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				if os.RemoveAll(filepath.Join(imagesDir, e.Name())) == nil {
					removed = append(removed, "abandoned build "+e.Name())
				}
			}
			continue
		}
		id := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".lock"), ".finalize.log")
		if !validID(id) {
			continue // pins
		}
		// An image built in the last hour may be about to be finalized and
		// pinned by another skerry process.
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < time.Hour {
			keep[id] = true
		}
		if keep[id] {
			if m, err := readManifest(filepath.Join(imagesDir, id)); err == nil {
				for _, p := range m.Packages {
					keepFiles[p+".apk"] = true
				}
				keepFiles["linux-virt-"+m.Kernel+".apk"] = true
			}
			continue
		}
		if err := os.RemoveAll(filepath.Join(imagesDir, e.Name())); err != nil {
			return removed, err
		}
		if e.IsDir() {
			removed = append(removed, "image "+id[:12])
		}
	}
	// Downloaded packages only image builds use.
	err = filepath.WalkDir(filepath.Join(b.Dir, "packages"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".apk") || keepFiles[d.Name()] {
			return nil
		}
		if os.Remove(path) == nil {
			removed = append(removed, "package "+d.Name())
		}
		return nil
	})
	return removed, err
}
