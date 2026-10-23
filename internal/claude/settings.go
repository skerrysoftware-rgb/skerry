// Package claude installs skerry's hooks into a Claude Code settings file.
//
// The settings file belongs to the user, so edits are surgical: top-level
// keys keep their order and contents, only the three hook events skerry uses
// are touched, and hooks from other tools are left alone.
package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
)

// Hooks maps each Claude event to the skerry hook subcommand that serves it.
var Hooks = []struct{ Event, Sub string }{
	{"WorktreeCreate", "worktree-create"},
	{"WorktreeRemove", "worktree-remove"},
	{"SessionStart", "session-start"},
}

// exclusive events can only have one handler: Claude uses one worktree path.
var exclusive = map[string]bool{"WorktreeCreate": true, "WorktreeRemove": true}

// ErrConflict means another tool already handles a worktree event.
var ErrConflict = errors.New("another worktree hook is configured")

// Plan computes the new settings file contents.
//
// current is the existing file (nil if absent). command is the quoted path
// to the skerry binary. With install false, skerry's hooks are removed.
// With force, other tools' WorktreeCreate/WorktreeRemove hooks are replaced.
func Plan(current []byte, command string, install, force bool) ([]byte, error) {
	top, err := parseObject(current)
	if err != nil {
		return nil, fmt.Errorf("settings file is not a JSON object: %w", err)
	}
	hooks, err := parseObject(top.get("hooks"))
	if err != nil {
		return nil, fmt.Errorf(`"hooks" is not a JSON object: %w`, err)
	}

	for _, h := range Hooks {
		groups, err := parseArray(hooks.get(h.Event))
		if err != nil {
			return nil, fmt.Errorf("hooks.%s is not an array: %w", h.Event, err)
		}
		kept := groups[:0]
		for _, g := range groups {
			mine, err := isSkerryGroup(g)
			if err != nil {
				return nil, fmt.Errorf("hooks.%s: %w", h.Event, err)
			}
			if !mine {
				kept = append(kept, g)
			}
		}
		groups = kept

		if install {
			if exclusive[h.Event] && len(groups) > 0 {
				if !force {
					return nil, fmt.Errorf("%w for %s; rerun with --force to replace it", ErrConflict, h.Event)
				}
				groups = nil
			}
			type hookEntry struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			group, err := json.Marshal(struct {
				Hooks []hookEntry `json:"hooks"`
			}{[]hookEntry{{"command", command + " hook " + h.Sub}}})
			if err != nil {
				return nil, err
			}
			groups = append(groups, group)
		}

		if len(groups) == 0 {
			hooks.remove(h.Event)
			continue
		}
		raw, err := json.Marshal(groups)
		if err != nil {
			return nil, err
		}
		hooks.set(h.Event, raw)
	}

	if len(hooks) == 0 {
		top.remove("hooks")
	} else {
		raw, err := hooks.marshal()
		if err != nil {
			return nil, err
		}
		top.set("hooks", raw)
	}
	out, err := top.marshal()
	if err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, out, "", "  "); err != nil {
		return nil, err
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), nil
}

// isSkerryGroup reports whether every hook in a matcher group runs skerry.
func isSkerryGroup(raw json.RawMessage) (bool, error) {
	var g struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return false, fmt.Errorf("unexpected hook entry: %w", err)
	}
	if len(g.Hooks) == 0 {
		return false, nil
	}
	for _, h := range g.Hooks {
		if !IsSkerryCommand(h.Command) {
			return false, nil
		}
	}
	return true, nil
}

// IsSkerryCommand recognizes a hook command written by skerry.
func IsSkerryCommand(cmd string) bool {
	for _, h := range Hooks {
		if strings.HasSuffix(cmd, " hook "+h.Sub) && strings.Contains(cmd, "skerry") {
			return true
		}
	}
	return false
}

// Write replaces the settings file at path with data, after copying the old
// file to a timestamped backup next to it (returned; empty if there was no
// file). A symlinked settings file, as dotfile managers create, stays a
// symlink: the file it points to is replaced. The write is atomic.
func Write(path string, data []byte, stamp string) (backup string, err error) {
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	mode := os.FileMode(0o600)
	old, err := os.ReadFile(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		if info, err := os.Stat(target); err == nil {
			mode = info.Mode().Perm()
		}
		backup = target + ".skerry-backup-" + stamp
		if err := os.WriteFile(backup, old, 0o600); err != nil {
			return "", err
		}
	}
	return backup, fsutil.WriteFileAtomic(target, data, mode)
}

// object is a JSON object that keeps its keys in order.
type object []member

type member struct {
	key   string
	value json.RawMessage
}

func parseObject(data []byte) (object, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return object{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("expected an object")
	}
	var obj object
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		obj.set(key, value)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after object")
	}
	return obj, nil
}

func parseArray(data json.RawMessage) ([]json.RawMessage, error) {
	if data == nil {
		return nil, nil
	}
	var arr []json.RawMessage
	err := json.Unmarshal(data, &arr)
	return arr, err
}

func (o object) get(key string) json.RawMessage {
	for _, m := range o {
		if m.key == key {
			return m.value
		}
	}
	return nil
}

func (o *object) set(key string, value json.RawMessage) {
	for i := range *o {
		if (*o)[i].key == key {
			(*o)[i].value = value
			return
		}
	}
	*o = append(*o, member{key, value})
}

func (o *object) remove(key string) {
	for i := range *o {
		if (*o)[i].key == key {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

func (o object) marshal() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
