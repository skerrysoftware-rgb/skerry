package image

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxKernel bounds an uncompressed kernel image.
const maxKernel = 256 << 20

// UnpackKernel returns the plain arm64 kernel Image that Apple's
// Virtualization framework boots. Alpine ships its kernel as an EFI "zboot"
// image: a small EFI stub followed by the compressed kernel. A kernel that is
// already a plain Image is returned unchanged.
func UnpackKernel(data []byte) ([]byte, error) {
	if isArm64Image(data) {
		return data, nil
	}
	// zboot header: "MZ" at 0, "zimg" at 4, payload offset and size
	// (little-endian u32) at 8 and 12, compression name at 24.
	if len(data) < 64 || string(data[0:2]) != "MZ" || string(data[4:8]) != "zimg" {
		return nil, errors.New("not an arm64 Image or EFI zboot kernel")
	}
	offset := binary.LittleEndian.Uint32(data[8:12])
	size := binary.LittleEndian.Uint32(data[12:16])
	comp := string(bytes.TrimRight(data[24:32], "\x00"))
	end := uint64(offset) + uint64(size)
	if end > uint64(len(data)) {
		return nil, fmt.Errorf("zboot payload (%d+%d) runs past the file (%d bytes)", offset, size, len(data))
	}
	if comp != "gzip" {
		return nil, fmt.Errorf("zboot compression %q is not supported", comp)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data[offset:end]))
	if err != nil {
		return nil, fmt.Errorf("zboot payload: %w", err)
	}
	image, err := io.ReadAll(io.LimitReader(zr, maxKernel+1))
	if err != nil {
		return nil, fmt.Errorf("zboot payload: %w", err)
	}
	if len(image) > maxKernel {
		return nil, errors.New("kernel is larger than 256 MiB")
	}
	if !isArm64Image(image) {
		return nil, errors.New("zboot payload is not an arm64 kernel Image")
	}
	return image, nil
}

// isArm64Image checks for the arm64 boot header magic "ARM\x64" at 56.
func isArm64Image(data []byte) bool {
	return len(data) >= 64 && string(data[56:60]) == "ARM\x64"
}
