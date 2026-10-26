// instancekey creates an Ed25519 key and signed CSR on the workload host.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func run() error {
	out := flag.String("out", "", "new directory for instance.key and instance.csr")
	flag.Parse()
	if *out == "" {
		return fmt.Errorf("-out must name a new directory")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err = os.Mkdir(*out, 0700); err != nil {
		return err
	}
	for _, entry := range []struct {
		name, kind string
		data       []byte
	}{{"instance.key", "PRIVATE KEY", der}, {"instance.csr", "CERTIFICATE REQUEST", csr}} {
		file, err := os.OpenFile(filepath.Join(*out, entry.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		err = pem.Encode(file, &pem.Block{Type: entry.kind, Bytes: entry.data})
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	fmt.Println("Created instance.key and instance.csr in", *out)
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
