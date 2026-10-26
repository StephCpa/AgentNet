package execution

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/agentnet/agentnet/internal/canonical"
)

type PayloadSource interface {
	Load(context.Context, string) ([]byte, error)
}

// Files is the local immutable parameter store. Provision it on a trusted,
// access-controlled volume; do not use an attacker-writable directory or symlinks.
// The ledger and notification payloads contain only the digest, never parameters.
type Files struct{ Root string }

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (f Files) Load(ctx context.Context, digest string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !digestPattern.MatchString(digest) {
		return nil, ErrInvalid
	}
	file, err := os.Open(filepath.Join(f.Root, digest[7:]+".json"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, ErrInvalid
	}
	got, err := canonical.Digest(b)
	if err != nil || got != digest {
		return nil, ErrInvalid
	}
	return b, nil
}
func (f Files) Put(ctx context.Context, b []byte) (string, error) {
	if len(b) > 1<<20 {
		return "", ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	b, err := canonical.Bytes(b)
	if err != nil {
		return "", err
	}
	digest, err := canonical.Digest(b)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(f.Root, 0700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(f.Root, ".parameter-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	dest := filepath.Join(f.Root, digest[7:]+".json")
	// Hard-link publication is atomic and cannot overwrite an existing binding.
	if err = os.Link(name, dest); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if _, err = f.Load(ctx, digest); err != nil {
			return "", err
		}
	}
	return digest, nil
}
