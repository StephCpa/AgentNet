// Package canonical applies RFC 8785 plus AgentNet's safe-integer input profile.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"unicode/utf8"

	jcs "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

func Bytes(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, fmt.Errorf("invalid UTF-8 JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if n, ok := token.(json.Number); ok {
			f, err := n.Float64()
			if err != nil || math.IsInf(f, 0) || (math.Trunc(f) == f && math.Abs(f) >= 9007199254740992) {
				return nil, fmt.Errorf("number outside AgentNet profile: %s", n)
			}
		}
	}
	// Upstream expects an object/array at root. Wrapping also supports scalar JSON.
	wrapped := append([]byte{'['}, raw...)
	wrapped = append(wrapped, ']')
	out, err := jcs.Transform(wrapped) // also rejects duplicate keys and unpaired surrogates
	if err != nil {
		return nil, err
	}
	return out[1 : len(out)-1], nil
}

func Digest(raw []byte) (string, error) {
	b, err := Bytes(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
