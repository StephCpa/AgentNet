package credentialclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialFilePublicationAndLock(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "current.pem")
	if err := writeFile(p, []byte("complete-old")); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(p, []byte("complete-new")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "complete-new" {
		t.Fatal("replacement failed", err)
	}
	lock := filepath.Join(dir, "state.lock")
	unlock, err := lockFile(lock)
	if err != nil {
		t.Fatal(err)
	}
	if second, e := lockFile(lock); e == nil {
		second()
		unlock()
		t.Fatal("two writers acquired same state")
	}
	unlock()
	again, err := lockFile(lock)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
