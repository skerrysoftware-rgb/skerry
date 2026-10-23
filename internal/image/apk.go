// Package image builds workspace images from signed apk packages: the
// guest system from Wolfi (glibc-based, so native modules built for
// ordinary Linux work) and the kernel from Alpine.
//
// Nothing enters an image unverified. Trust starts at each source's signing
// keys, pinned in this package (keys/<source>/). Alpine's were checked through
// its release GPG key; Wolfi's matched on its package server and in its
// GitHub repository. From there:
//
//	signed APKINDEX → each package's control-section hash (index C: field)
//	→ the control section's datahash → the data section
package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // Alpine's v2 package format signs and indexes with SHA-1
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

//go:embed keys
var keyFiles embed.FS

// Keys maps a signing key's file name to the key.
type Keys map[string]*rsa.PublicKey

// TrustedKeys returns the pinned signing keys for one source ("alpine" or
// "wolfi"). Each source is verified only with its own keys.
func TrustedKeys(source string) (Keys, error) {
	entries, err := keyFiles.ReadDir("keys/" + source)
	if err != nil {
		return nil, fmt.Errorf("no pinned keys for %q: %w", source, err)
	}
	keys := Keys{}
	for _, e := range entries {
		data, err := keyFiles.ReadFile("keys/" + source + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		key, err := parsePublicKey(data)
		if err != nil {
			return nil, fmt.Errorf("pinned key %s: %w", e.Name(), err)
		}
		keys[e.Name()] = key
	}
	return keys, nil
}

func parsePublicKey(data []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return key, nil
}

// splitGzip splits concatenated gzip members and returns each member's raw
// bytes. Alpine's formats sign and hash the raw member bytes.
func splitGzip(blob []byte) ([][]byte, error) {
	var parts [][]byte
	for offset := 0; offset < len(blob); {
		r := bytes.NewReader(blob[offset:])
		// bytes.Reader is an io.ByteReader, so the decompressor reads exactly
		// one member and the reader's position marks where it ends.
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("gzip member at %d: %w", offset, err)
		}
		zr.Multistream(false)
		if _, err := io.Copy(io.Discard, zr); err != nil {
			return nil, fmt.Errorf("gzip member at %d: %w", offset, err)
		}
		used := int(r.Size()) - r.Len()
		parts = append(parts, blob[offset:offset+used])
		offset += used
	}
	return parts, nil
}

// Limits on files read from small archive members.
const (
	maxControlFile = 1 << 20  // signatures, .PKGINFO, install scripts
	maxIndexFile   = 64 << 20 // APKINDEX text
)

// readTarFiles returns the regular files in a gzipped tar member, refusing
// any file larger than limit.
func readTarFiles(member []byte, limit int64) (map[string][]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(member))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > limit {
			return nil, fmt.Errorf("%s is too large (%d bytes)", hdr.Name, hdr.Size)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[hdr.Name] = data
	}
}

// verifySignature checks that the signature member signs signed with a
// trusted key, and returns the key's name.
func verifySignature(keys Keys, sigMember, signed []byte) (string, error) {
	files, err := readTarFiles(sigMember, maxControlFile)
	if err != nil {
		return "", fmt.Errorf("signature section: %w", err)
	}
	if len(files) != 1 {
		return "", fmt.Errorf("signature section has %d files, want 1", len(files))
	}
	for name, sig := range files {
		var (
			hash   crypto.Hash
			digest []byte
			key    string
		)
		switch {
		case strings.HasPrefix(name, ".SIGN.RSA256."):
			key = strings.TrimPrefix(name, ".SIGN.RSA256.")
			sum := sha256.Sum256(signed)
			hash, digest = crypto.SHA256, sum[:]
		case strings.HasPrefix(name, ".SIGN.RSA."):
			key = strings.TrimPrefix(name, ".SIGN.RSA.")
			sum := sha1.Sum(signed) //nolint:gosec // format-mandated
			hash, digest = crypto.SHA1, sum[:]
		default:
			return "", fmt.Errorf("unsupported signature %q", name)
		}
		pub, ok := keys[key]
		if !ok {
			return "", fmt.Errorf("signed by untrusted key %q", key)
		}
		if err := rsa.VerifyPKCS1v15(pub, hash, digest, sig); err != nil {
			return "", fmt.Errorf("bad signature by %s: %w", key, err)
		}
		return key, nil
	}
	panic("unreachable")
}

// VerifyIndex checks a signed APKINDEX.tar.gz and returns the index text.
func VerifyIndex(keys Keys, blob []byte) (string, error) {
	parts, err := splitGzip(blob)
	if err != nil {
		return "", fmt.Errorf("APKINDEX: %w", err)
	}
	if len(parts) != 2 {
		return "", fmt.Errorf("APKINDEX: %d gzip members, want 2", len(parts))
	}
	if _, err := verifySignature(keys, parts[0], parts[1]); err != nil {
		return "", fmt.Errorf("APKINDEX: %w", err)
	}
	files, err := readTarFiles(parts[1], maxIndexFile)
	if err != nil {
		return "", fmt.Errorf("APKINDEX: %w", err)
	}
	index, ok := files["APKINDEX"]
	if !ok {
		return "", errors.New("APKINDEX: no APKINDEX file inside")
	}
	return string(index), nil
}

// Verified is a package whose contents have been checked against a signed index.
type Verified struct {
	Package Package
	// Scripts holds install scripts by name (".post-install", ...).
	Scripts map[string][]byte
	// data is the data section: a gzipped tar of the package's files.
	data []byte
}

// VerifyPackage checks an .apk against its signed index entry.
func VerifyPackage(keys Keys, pkg Package, blob []byte) (*Verified, error) {
	name := pkg.Name + "-" + pkg.Version
	if int64(len(blob)) != pkg.Size {
		return nil, fmt.Errorf("%s: size %d, index says %d", name, len(blob), pkg.Size)
	}
	parts, err := splitGzip(blob)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	// Alpine signs each package (signature, control, data); Wolfi relies on
	// the signed index alone (control, data). Either way the chain below
	// is what establishes trust.
	var sig, control, data []byte
	switch len(parts) {
	case 3:
		sig, control, data = parts[0], parts[1], parts[2]
	case 2:
		control, data = parts[0], parts[1]
	default:
		return nil, fmt.Errorf("%s: %d gzip members, want 2 or 3", name, len(parts))
	}

	want, err := controlHash(pkg.Checksum)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if got := sha1.Sum(control); !bytes.Equal(got[:], want) { //nolint:gosec // format-mandated
		return nil, fmt.Errorf("%s: control section does not match the signed index", name)
	}
	if sig != nil {
		// Redundant with the index check, but cheap: the package is signed too.
		if _, err := verifySignature(keys, sig, control); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}

	files, err := readTarFiles(control, maxControlFile)
	if err != nil {
		return nil, fmt.Errorf("%s: control section: %w", name, err)
	}
	datahash, err := pkginfoField(files[".PKGINFO"], "datahash")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != datahash {
		return nil, fmt.Errorf("%s: data section does not match datahash", name)
	}

	scripts := map[string][]byte{}
	for file, content := range files {
		if strings.HasPrefix(file, ".") && file != ".PKGINFO" && path.Base(file) == file {
			scripts[file] = content
		}
	}
	return &Verified{Package: pkg, Scripts: scripts, data: data}, nil
}

// controlHash decodes an index C: field ("Q1" + base64 SHA-1).
func controlHash(field string) ([]byte, error) {
	if !strings.HasPrefix(field, "Q1") {
		return nil, fmt.Errorf("unsupported checksum %q", field)
	}
	sum, err := base64.StdEncoding.DecodeString(field[2:])
	if err != nil || len(sum) != sha1.Size {
		return nil, fmt.Errorf("malformed checksum %q", field)
	}
	return sum, nil
}

func pkginfoField(pkginfo []byte, key string) (string, error) {
	for _, line := range strings.Split(string(pkginfo), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf(".PKGINFO has no %s", key)
}
