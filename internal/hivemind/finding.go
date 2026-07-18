package hivemind

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// Finding is one distilled, shareable insight — a lesson, decision, or discovery,
// never raw code or a transcript. It is signed by its author so peers can verify
// authenticity and filter by author. The JSON form is the open wire format other
// harnesses interoperate with (see docs/hivemind.md).
type Finding struct {
	ID          string    `json:"id"`                     // content hash (hex), set by Sign
	Author      string    `json:"author"`                 // author public key (hex)
	Created     time.Time `json:"created"`                // UTC
	Tags        []string  `json:"tags,omitempty"`         // normalized, for matching
	Title       string    `json:"title"`                  // one-line summary
	Body        string    `json:"body"`                   // the insight
	ProjectHint string    `json:"project_hint,omitempty"` // coarse topic, NOT a path/secret
	Sig         string    `json:"sig"`                    // signature over the canonical form (hex)
}

// canonical is the deterministic byte form that is hashed (for ID) and signed.
// It excludes ID and Sig so both are reproducible from the content.
func (f Finding) canonical() []byte {
	c := struct {
		Author      string    `json:"author"`
		Created     time.Time `json:"created"`
		Tags        []string  `json:"tags"`
		Title       string    `json:"title"`
		Body        string    `json:"body"`
		ProjectHint string    `json:"project_hint"`
	}{f.Author, f.Created.UTC(), normTags(f.Tags), f.Title, f.Body, f.ProjectHint}
	b, _ := json.Marshal(c)
	return b
}

// normTags lowercases, trims, dedupes, and sorts tags for stable matching.
func normTags(tags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// Sign stamps the finding with the author, a content-hash ID, and a signature.
func (f *Finding) Sign(id Identity) {
	f.Author = id.AuthorID()
	f.Tags = normTags(f.Tags)
	if f.Created.IsZero() {
		f.Created = time.Now().UTC()
	}
	c := f.canonical()
	sum := sha256.Sum256(c)
	f.ID = hex.EncodeToString(sum[:])
	f.Sig = hex.EncodeToString(ed25519.Sign(id.Priv, c))
}

// Verify checks that the signature is valid for the stated author and that the
// ID matches the content — rejecting forged or tampered findings.
func (f Finding) Verify() error {
	pub, err := hex.DecodeString(f.Author)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("hivemind: bad author key")
	}
	sig, err := hex.DecodeString(f.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("hivemind: bad signature encoding")
	}
	c := f.canonical()
	if !ed25519.Verify(ed25519.PublicKey(pub), c, sig) {
		return errors.New("hivemind: signature verification failed")
	}
	sum := sha256.Sum256(c)
	if f.ID != hex.EncodeToString(sum[:]) {
		return errors.New("hivemind: id does not match content")
	}
	return nil
}
