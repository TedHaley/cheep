// Package hivemind is the decentralized peer-findings core: a node identity, a
// signed "finding" record, and a local pool of findings received from peers. It
// is deliberately transport-agnostic — the libp2p networking layer and the
// cheep TUI (and any other harness, via headless subcommands) build on top of
// these primitives. See docs/hivemind.md.
package hivemind

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/TedHaley/cheep/internal/config"
)

// dir is ~/.cheep/hivemind.
func dir() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "hivemind"), nil
}

// Identity is this node's signing keypair. The public key is the author id
// stamped on every finding it shares, so peers can trust, filter, or mute by
// author. The same ed25519 seed can later seed the libp2p peer identity.
type Identity struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// AuthorID is the hex-encoded public key — the stable identifier for this node.
func (id Identity) AuthorID() string { return hex.EncodeToString(id.Pub) }

// LoadIdentity loads the node keypair from ~/.cheep/hivemind/identity.key,
// creating and persisting one on first use.
func LoadIdentity() (Identity, error) {
	d, err := dir()
	if err != nil {
		return Identity{}, err
	}
	path := filepath.Join(d, "identity.key")
	if b, err := os.ReadFile(path); err == nil && len(b) == ed25519.PrivateKeySize {
		priv := ed25519.PrivateKey(b)
		return Identity{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return Identity{}, err
	}
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return Identity{}, err
	}
	return Identity{Priv: priv, Pub: pub}, nil
}
