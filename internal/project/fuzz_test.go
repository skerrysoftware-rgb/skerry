package project

import (
	"os"
	"path/filepath"
	"testing"
)

// skerry.yaml and the files detection reads are repository content, which
// the VM can rewrite at will.
func FuzzLoad(f *testing.F) {
	f.Add("services: [postgres, redis]\nports: [3000]\n", `{"dependencies":{"pg":"8"}}`)
	f.Add("services: !!binary aGk=\n", "{")
	f.Add("ports: [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17]\n", "")
	f.Fuzz(func(t *testing.T, config, pkg string) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, FileName), []byte(config), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644)
		p, err := Load(dir)
		if err != nil {
			return
		}
		if len(p.Ports) > maxPorts {
			t.Errorf("%d ports accepted", len(p.Ports))
		}
		for _, port := range p.Ports {
			if port == 0 {
				t.Error("port 0 accepted")
			}
		}
	})
}
