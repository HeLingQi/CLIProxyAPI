// Package antigravity provides OAuth2 authentication functionality for the Antigravity provider.
package antigravity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCECodes is one RFC 7636 code verifier and its S256 challenge. The official
// client creates the pair before opening the consent page and presents the
// verifier on the code exchange, so an Antigravity grant obtained without PKCE
// is distinguishable from a real client's grant for the lifetime of the refresh
// token. A nil *PKCECodes disables PKCE and keeps the pre-existing requests.
type PKCECodes struct {
	Verifier  string
	Challenge string
}

// GeneratePKCECodes builds a fresh verifier/challenge pair.
func GeneratePKCECodes() (*PKCECodes, error) {
	raw := make([]byte, 48)
	if _, errRead := rand.Read(raw); errRead != nil {
		return nil, fmt.Errorf("read antigravity PKCE verifier: %w", errRead)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return &PKCECodes{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}
