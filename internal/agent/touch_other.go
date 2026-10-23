//go:build !linux

package agent

// touchAll only means something in the guest.
func touchAll(workspace string, rels []string) {}
