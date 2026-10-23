package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// Keys are one workspace's SSH keys, as the host holds them.
type Keys struct {
	// Client authenticates the host to the agent. It never leaves the host.
	Client ssh.Signer
	// HostPublic is the agent's key; the host accepts no other.
	HostPublic ssh.PublicKey
}

// clientKeyFile lives in the host-only state directory, never in the share.
const clientKeyFile = "client_key"

// EnsureKeys loads a workspace's keys, generating them on first use. The
// client key goes in stateDir (host only); the agent's private host key and
// the client's public key go in shareDir, which the guest reads.
func EnsureKeys(stateDir, shareDir string) (Keys, error) {
	clientPath := filepath.Join(stateDir, clientKeyFile)
	hostPath := filepath.Join(shareDir, HostKeyFile)
	if _, err := os.Stat(clientPath); errors.Is(err, os.ErrNotExist) {
		if err := generateKeys(clientPath, hostPath, filepath.Join(shareDir, AuthorizedKeyFile)); err != nil {
			return Keys{}, err
		}
	}
	client, err := loadSigner(clientPath)
	if err != nil {
		return Keys{}, err
	}
	host, err := loadSigner(hostPath)
	if err != nil {
		return Keys{}, err
	}
	return Keys{Client: client, HostPublic: host.PublicKey()}, nil
}

func generateKeys(clientPath, hostPath, authorizedPath string) error {
	clientPub, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writePrivate(hostPath, hostKey); err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		return err
	}
	if err := os.WriteFile(authorizedPath, ssh.MarshalAuthorizedKey(pub), 0o600); err != nil {
		return err
	}
	// Written last: its presence means the set is complete.
	return writePrivate(clientPath, clientKey)
}

func writePrivate(path string, key ed25519.PrivateKey) error {
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
}

func loadSigner(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return signer, nil
}
