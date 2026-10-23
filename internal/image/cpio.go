package image

import (
	"fmt"
	"io"
	"strings"
)

// CPIO writes an initramfs in the "newc" format the Linux kernel unpacks.
// Every entry is owned by root.
type CPIO struct {
	w     io.Writer
	inode uint32
	err   error
}

// NewCPIO starts an archive on w.
func NewCPIO(w io.Writer) *CPIO { return &CPIO{w: w, inode: 1} }

// Dir adds a directory.
func (c *CPIO) Dir(name string, perm uint32) { c.entry(name, 0o040000|perm, nil) }

// File adds a regular file.
func (c *CPIO) File(name string, perm uint32, data []byte) { c.entry(name, 0o100000|perm, data) }

// Close writes the trailer and reports the first error.
func (c *CPIO) Close() error {
	c.entry("TRAILER!!!", 0, nil)
	return c.err
}

func (c *CPIO) entry(name string, mode uint32, data []byte) {
	if c.err != nil {
		return
	}
	if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		c.err = fmt.Errorf("cpio: unsafe name %q", name)
		return
	}
	nlink := uint32(1)
	if mode&0o040000 != 0 {
		nlink = 2
	}
	ino := c.inode
	c.inode++
	// magic, ino, mode, uid, gid, nlink, mtime, filesize, devmajor, devminor,
	// rdevmajor, rdevminor, namesize, check
	hdr := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		ino, mode, 0, 0, nlink, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
	c.write([]byte(hdr))
	c.write(append([]byte(name), 0))
	c.pad(len(hdr) + len(name) + 1)
	c.write(data)
	c.pad(len(data))
}

func (c *CPIO) write(b []byte) {
	if c.err == nil {
		_, c.err = c.w.Write(b)
	}
}

func (c *CPIO) pad(n int) {
	if r := n % 4; r != 0 {
		c.write(make([]byte, 4-r))
	}
}
