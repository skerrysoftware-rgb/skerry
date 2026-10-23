package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // matches the format under test
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func gzTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sign(t *testing.T, key *rsa.PrivateKey, keyName string, signed []byte) []byte {
	t.Helper()
	sum := sha1.Sum(signed) //nolint:gosec // format-mandated
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return gzTar(t, tarEntry{name: ".SIGN.RSA." + keyName, body: string(sig)})
}

type fixture struct {
	keys    Keys
	key     *rsa.PrivateKey
	keyName string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{keys: Keys{"test.rsa.pub": &key.PublicKey}, key: key, keyName: "test.rsa.pub"}
}

// pkg builds a signed .apk and its index entry from data entries.
func (f fixture) pkg(t *testing.T, name string, data ...tarEntry) (Package, []byte) {
	t.Helper()
	dataMember := gzTar(t, data...)
	sum := sha256.Sum256(dataMember)
	control := gzTar(t,
		tarEntry{name: ".PKGINFO", body: "pkgname = " + name + "\npkgver = 1.0-r0\ndatahash = " + hex.EncodeToString(sum[:]) + "\n"},
		tarEntry{name: ".post-install", body: "#!/bin/sh\necho installed\n", mode: 0o755},
	)
	blob := append(append(f.sign(t, control), control...), dataMember...)
	csum := sha1.Sum(control) //nolint:gosec // format-mandated
	return Package{
		Name: name, Version: "1.0-r0", Size: int64(len(blob)),
		Checksum: "Q1" + base64.StdEncoding.EncodeToString(csum[:]),
	}, blob
}

func (f fixture) sign(t *testing.T, signed []byte) []byte { return sign(t, f.key, f.keyName, signed) }

func TestVerifyIndex(t *testing.T) {
	f := newFixture(t)
	body := gzTar(t, tarEntry{name: "APKINDEX", body: "P:a\nV:1-r0\nC:Q1x\nS:1\n"}, tarEntry{name: "DESCRIPTION", body: "test"})
	good := append(f.sign(t, body), body...)

	text, err := VerifyIndex(f.keys, good)
	if err != nil || !strings.Contains(text, "P:a") {
		t.Fatalf("VerifyIndex = %q, %v", text, err)
	}

	other := newFixture(t)
	if _, err := VerifyIndex(f.keys, append(other.sign(t, body), body...)); err == nil {
		t.Error("accepted an index signed by an unknown key")
	}
	tampered := append([]byte{}, good...)
	tampered[len(tampered)-30] ^= 0xff
	if _, err := VerifyIndex(f.keys, tampered); err == nil {
		t.Error("accepted a tampered index")
	}
	if _, err := VerifyIndex(f.keys, body); err == nil {
		t.Error("accepted an unsigned index")
	}
}

func TestVerifyPackage(t *testing.T) {
	f := newFixture(t)
	entry, blob := f.pkg(t, "hello", tarEntry{name: "usr/bin/hello", body: "#!/bin/sh\n", mode: 0o755})

	v, err := VerifyPackage(f.keys, entry, blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Scripts[".post-install"]) != "#!/bin/sh\necho installed\n" {
		t.Errorf("scripts = %v", v.Scripts)
	}

	t.Run("tampered data", func(t *testing.T) {
		bad := append([]byte{}, blob...)
		bad[len(bad)-30] ^= 0xff
		if _, err := VerifyPackage(f.keys, entry, bad); err == nil {
			t.Fatal("accepted tampered data")
		}
	})
	t.Run("swapped data", func(t *testing.T) {
		_, other := f.pkg(t, "hello", tarEntry{name: "usr/bin/hello", body: "evil"})
		e := entry
		e.Size = int64(len(other))
		if _, err := VerifyPackage(f.keys, e, other); err == nil {
			t.Fatal("accepted a package whose control section differs from the index")
		}
	})
	t.Run("wrong size", func(t *testing.T) {
		e := entry
		e.Size++
		if _, err := VerifyPackage(f.keys, e, blob); err == nil {
			t.Fatal("accepted a size mismatch")
		}
	})
	t.Run("untrusted signer", func(t *testing.T) {
		if _, err := VerifyPackage(newFixture(t).keys, entry, blob); err == nil {
			t.Fatal("accepted a package signed by an unknown key")
		}
	})
}

func TestExtract(t *testing.T) {
	f := newFixture(t)
	entry, blob := f.pkg(t, "tree",
		tarEntry{name: "usr/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "usr/bin/tool", body: "binary", mode: 0o4755}, // setuid dropped
		tarEntry{name: "bin/sh", typ: tar.TypeSymlink, link: "/bin/busybox"},
		tarEntry{name: "usr/bin/tool2", typ: tar.TypeLink, link: "usr/bin/tool"},
	)
	v, err := VerifyPackage(f.keys, entry, blob)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := v.Extract(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "usr/bin/tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 || info.Mode().Perm() != 0o755 {
		t.Errorf("tool mode = %v", info.Mode())
	}
	if link, err := os.Readlink(filepath.Join(dir, "bin/sh")); err != nil || link != "/bin/busybox" {
		t.Errorf("bin/sh -> %q, %v", link, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "usr/bin/tool2")); string(got) != "binary" {
		t.Errorf("hard link content = %q", got)
	}
}

func TestExtractCannotEscape(t *testing.T) {
	f := newFixture(t)
	for name, entries := range map[string][]tarEntry{
		"dot-dot":           {{name: "../escaped", body: "x"}},
		"nested dot-dot":    {{name: "usr/../../escaped", body: "x"}},
		"through symlink":   {{name: "out", typ: tar.TypeSymlink, link: "OUTSIDE"}, {name: "out/escaped", body: "x"}},
		"hard link out":     {{name: "h", typ: tar.TypeLink, link: "../outside-file"}},
		"symlink then file": {{name: "f", typ: tar.TypeSymlink, link: "OUTSIDE/escaped"}, {name: "f", body: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			outside := filepath.Join(parent, "outside")
			if err := os.Mkdir(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(parent, "outside-file"), []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			for i := range entries {
				entries[i].link = strings.ReplaceAll(entries[i].link, "OUTSIDE", outside)
			}
			entry, blob := f.pkg(t, "evil", entries...)
			v, err := VerifyPackage(f.keys, entry, blob)
			if err != nil {
				t.Fatal(err)
			}
			imageDir := filepath.Join(parent, "image")
			if err := os.Mkdir(imageDir, 0o755); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(imageDir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			_ = v.Extract(root) // may fail or skip; either way nothing may land outside

			if _, err := os.Stat(filepath.Join(parent, "escaped")); err == nil {
				t.Error("wrote next to the image directory")
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Errorf("wrote into a directory outside the image: %v", entries)
			}
			if got, _ := os.ReadFile(filepath.Join(parent, "outside-file")); string(got) != "keep" {
				t.Error("modified a file outside the image")
			}
		})
	}
}

func TestResolve(t *testing.T) {
	pkgs, err := ParseIndex(strings.Join([]string{
		"P:app\nV:1-r0\nC:Q1a\nS:1\nD:so:libz.so.1 /bin/sh !conflicting libbar>=2",
		"P:zlib\nV:1-r0\nC:Q1b\nS:1\np:so:libz.so.1=1",
		"P:busybox-binsh\nV:1-r0\nC:Q1c\nS:1\nk:100\nD:busybox\np:/bin/sh cmd:sh",
		"P:dash-binsh\nV:1-r0\nC:Q1d\nS:1\nk:60\np:/bin/sh cmd:sh",
		"P:busybox\nV:1-r0\nC:Q1e\nS:1",
		"P:libbar\nV:2-r0\nC:Q1f\nS:1\nD:app", // a cycle back to app
	}, "\n\n"), "main")
	if err != nil {
		t.Fatal(err)
	}
	order, err := NewResolver(pkgs).Resolve([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range order {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, " "); got != "zlib busybox busybox-binsh libbar app" {
		t.Errorf("order = %s", got)
	}
	if _, err := NewResolver(pkgs).Resolve([]string{"missing"}); err == nil {
		t.Error("resolved a package that does not exist")
	}
}

func TestParseIndexRejectsUnsafeNames(t *testing.T) {
	for _, entry := range []string{"P:../evil\nV:1\nC:Q1a\nS:1", "P:a\nV:1/../../x\nC:Q1a\nS:1"} {
		if _, err := ParseIndex(entry, "main"); err == nil {
			t.Errorf("accepted %q", entry)
		}
	}
}

func TestStaleIndexUsedWhenOffline(t *testing.T) {
	f := newFixture(t)
	body := gzTar(t, tarEntry{name: "APKINDEX", body: "P:a\nV:1-r0\nC:Q1x\nS:1\n"})
	signed := append(f.sign(t, body), body...)
	src := Source{Name: "test", Repos: []string{"https://example.invalid/os"}, Keys: f.keys}
	b := &Builder{Dir: t.TempDir(), Arch: "aarch64",
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("offline")
		})}}
	cached := filepath.Join(b.repoDir(src, src.Repos[0]), "APKINDEX.tar.gz")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, signed, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	os.Chtimes(cached, old, old)

	text, err := b.index(context.Background(), src, src.Repos[0])
	if err != nil || !strings.Contains(text, "P:a") {
		t.Fatalf("offline with a stale cached index: %q, %v", text, err)
	}
	// A cached index that doesn't verify is never used.
	os.WriteFile(cached, body, 0o644)
	os.Chtimes(cached, old, old)
	if _, err := b.index(context.Background(), src, src.Repos[0]); err == nil {
		t.Fatal("used an unsigned cached index")
	}
}

func TestEachSourceHasPinnedKeys(t *testing.T) {
	for _, name := range []string{"alpine", "wolfi"} {
		keys, err := TrustedKeys(name)
		if err != nil || len(keys) == 0 {
			t.Errorf("TrustedKeys(%s) = %d keys, %v", name, len(keys), err)
		}
	}
	if _, err := TrustedKeys("other"); err == nil {
		t.Error("keys for an unknown source")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.0-r0", "1.0-r0", 0},
		{"24.22.1-r2", "24.22.1-r10", -1},
		{"24.9.0-r0", "24.22.1-r0", -1},
		{"2.43-r13", "2.42-r20", 1},
		{"1.0_rc1-r0", "1.0-r0", -1},
		{"1.0_alpha2-r0", "1.0_beta1-r0", -1},
		{"1.0_p1-r0", "1.0-r0", 1},
		{"3.13.16_git20261016-r2", "3.13.16-r2", 1},
		{"1.2.3a-r0", "1.2.3-r0", 1},
		{"20230201-r30", "20230201-r9", 1},
		{"1.10-r0", "1.9-r0", 1},
	} {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := CompareVersions(tc.b, tc.a); got != -tc.want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestResolverPicksNewest(t *testing.T) {
	pkgs, err := ParseIndex(strings.Join([]string{
		"P:nodejs-24\nV:24.9.0-r0\nC:Q1a\nS:1\np:cmd:node",
		"P:nodejs-24\nV:24.22.1-r2\nC:Q1b\nS:1\np:cmd:node",
		"P:nodejs-24\nV:24.22.1-r1\nC:Q1c\nS:1\np:cmd:node",
	}, "\n\n"), "wolfi")
	if err != nil {
		t.Fatal(err)
	}
	r := NewResolver(pkgs)
	for _, name := range []string{"nodejs-24", "cmd:node"} {
		order, err := r.Resolve([]string{name})
		if err != nil || len(order) != 1 || order[0].Version != "24.22.1-r2" {
			t.Errorf("Resolve(%s) = %+v, %v; want 24.22.1-r2", name, order, err)
		}
	}
}

// Wolfi moves to a new glibc through a package that provides "glibc" at a
// higher version. Only one glibc may be installed, and a runtime asked for
// by name must not be replaced by a newer one another package would pull.
func TestResolverPicksOneProvider(t *testing.T) {
	pkgs, err := ParseIndex(strings.Join([]string{
		"P:glibc\nV:2.43-r13\nC:Q1a\nS:1\np:so:libc.so.6=6 so:libm.so.6=6",
		"P:glibc-2.44\nV:2.44-r9\nC:Q1b\nS:1\nk:244\np:glibc=2.44-r9 so:libc.so.6=6 so:libm.so.6=6",
		"P:nodejs-24\nV:24.22.1-r2\nC:Q1c\nS:1\nD:so:libm.so.6\np:nodejs=24.22.1-r2 cmd:node",
		"P:nodejs-26\nV:26.12.1-r2\nC:Q1d\nS:1\nD:so:libm.so.6\np:nodejs=26.12.1-r2 cmd:node",
		"P:npm\nV:12.0.2-r0\nC:Q1e\nS:1\nD:nodejs so:libc.so.6",
	}, "\n\n"), "wolfi")
	if err != nil {
		t.Fatal(err)
	}
	order, err := NewResolver(pkgs).Resolve([]string{"glibc", "nodejs-24", "npm"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range order {
		got = append(got, p.Name)
	}
	if strings.Join(got, " ") != "glibc-2.44 nodejs-24 npm" {
		t.Errorf("installed %v; want glibc-2.44 nodejs-24 npm", got)
	}
}

// Wolfi packages carry no per-package signature; the signed index's control
// hash still pins them.
func TestUnsignedPackageVerifiedThroughIndex(t *testing.T) {
	f := newFixture(t)
	data := gzTar(t, tarEntry{name: "usr/bin/x", body: "x"})
	sum := sha256.Sum256(data)
	control := gzTar(t, tarEntry{name: ".PKGINFO", body: "datahash = " + hex.EncodeToString(sum[:]) + "\n"})
	blob := append(append([]byte{}, control...), data...)
	csum := sha1.Sum(control) //nolint:gosec // format-mandated
	pkg := Package{Name: "x", Version: "1-r0", Size: int64(len(blob)), Checksum: "Q1" + base64.StdEncoding.EncodeToString(csum[:])}
	if _, err := VerifyPackage(f.keys, pkg, blob); err != nil {
		t.Fatalf("unsigned package with a matching index entry: %v", err)
	}
	evil := gzTar(t, tarEntry{name: "usr/bin/x", body: "evil"})
	tampered := append(append([]byte{}, control...), evil...)
	pkg.Size = int64(len(tampered))
	if _, err := VerifyPackage(f.keys, pkg, tampered); err == nil {
		t.Fatal("accepted swapped data in an unsigned package")
	}
}
