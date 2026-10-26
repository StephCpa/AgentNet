package credentialclient

import (
	"os"
	"path/filepath"
)

// writeFile publishes a complete file from a same-directory temporary file.
// State and the certificate/private-key bundle each have a single publication.
func writeFile(path string, body []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".renew-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(body)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceFile(name, path)
}
