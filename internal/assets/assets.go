// Package assets carries the guest agent inside the skerry binary, so host
// and guest are always the same version. `make` builds the agent into
// files/ before building skerry.
package assets

import (
	"embed"
	"errors"
	"io/fs"
	"runtime"
)

//go:embed all:files
var files embed.FS

// Agent returns the guest agent for the host's architecture.
func Agent() ([]byte, error) {
	name := "files/skerry-agent-linux-" + runtime.GOARCH
	data, err := files.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("this skerry was built without its guest agent; build it with `make`")
	}
	return data, err
}
