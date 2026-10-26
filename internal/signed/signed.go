// Package signed implements the versioned JCS/Ed25519 envelope shared by services.
package signed

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"

	"github.com/agentnet/agentnet/internal/canonical"
)

type Envelope struct {
	KeyID              string
	Payload, Signature []byte
}

var ErrInvalid = errors.New("invalid signed envelope")

func Decode(e Envelope, keys map[string]ed25519.PublicKey, out any) error {
	key, ok := keys[e.KeyID]
	if !ok || len(key) != ed25519.PublicKeySize {
		return ErrInvalid
	}
	b, err := canonical.Bytes(e.Payload)
	if err != nil || !ed25519.Verify(key, b, e.Signature) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return ErrInvalid
	}
	return nil
}
func Encode(keyID string, key ed25519.PrivateKey, v any) (Envelope, error) {
	if keyID == "" || len(key) != ed25519.PrivateKeySize {
		return Envelope{}, ErrInvalid
	}
	b, err := json.Marshal(v)
	if err != nil {
		return Envelope{}, err
	}
	b, err = canonical.Bytes(b)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{KeyID: keyID, Payload: b, Signature: ed25519.Sign(key, b)}, nil
}
