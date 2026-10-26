package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// QuickSpin's local certificate authority for https dev sites.
//
// FrankenPHP (Caddy's "internal" issuer) signs the site certificates with
// this CA's intermediate. Nothing is installed in the operating system: only
// QuickSpin's own Chromium trusts it, via --ignore-certificate-errors-spki-list
// with the CA's key hashes. Caddy would rotate its own intermediate every week
// (which would change the hash), so QuickSpin creates long-lived ones itself.

type localCA struct {
	RootCert, RootKey, IntCert, IntKey string   // PEM files
	SPKI                               []string // base64 SHA-256 of the public keys (root, intermediate)
}

func (a *App) caDir() string { return a.data("tls") }

// ensureCA loads the CA from data/tls, creating it on first use.
func (a *App) ensureCA() (*localCA, error) {
	a.caMu.Lock()
	defer a.caMu.Unlock()
	if a.ca != nil {
		return a.ca, nil
	}
	dir := a.caDir()
	ca := &localCA{
		RootCert: filepath.Join(dir, "root.crt"), RootKey: filepath.Join(dir, "root.key"),
		IntCert: filepath.Join(dir, "intermediate.crt"), IntKey: filepath.Join(dir, "intermediate.key"),
	}
	if !(exists(ca.RootCert) && exists(ca.RootKey) && exists(ca.IntCert) && exists(ca.IntKey)) || caExpiring(ca.IntCert) {
		if err := createCA(ca); err != nil {
			return nil, fmt.Errorf("creating the local HTTPS certificate authority: %w", err)
		}
		a.logf("Created QuickSpin's local HTTPS certificate authority (%s).", dir)
	}
	for _, f := range []string{ca.RootCert, ca.IntCert} {
		h, err := spkiHash(f)
		if err != nil {
			return nil, err
		}
		ca.SPKI = append(ca.SPKI, h)
	}
	a.ca = ca
	return ca, nil
}

func caExpiring(certFile string) bool {
	c, err := readCert(certFile)
	return err != nil || time.Until(c.NotAfter) < 30*24*time.Hour
}

func readCert(file string) (*x509.Certificate, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s is not a PEM certificate", file)
	}
	return x509.ParseCertificate(blk.Bytes)
}

func spkiHash(certFile string) (string, error) {
	c, err := readCert(certFile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

func createCA(ca *localCA) error {
	if err := os.MkdirAll(filepath.Dir(ca.RootCert), 0o700); err != nil {
		return err
	}
	now := time.Now().Add(-time.Hour)
	host, _ := os.Hostname()
	serial := func() *big.Int {
		n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
		return n
	}
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	rootTpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "QuickSpin Local Root CA (" + host + ")", Organization: []string{"QuickSpin"}},
		NotBefore:             now,
		NotAfter:              now.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTpl, rootTpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		return err
	}
	root, _ := x509.ParseCertificate(rootDER)
	intKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	intTpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "QuickSpin Local Intermediate CA", Organization: []string{"QuickSpin"}},
		NotBefore:             now,
		NotAfter:              now.AddDate(19, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	intDER, err := x509.CreateCertificate(rand.Reader, intTpl, root, &intKey.PublicKey, rootKey)
	if err != nil {
		return err
	}
	writePEM := func(file, typ string, der []byte, mode os.FileMode) error {
		return writeFileAtomicMode(file, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
	}
	rk, _ := x509.MarshalECPrivateKey(rootKey)
	ik, _ := x509.MarshalECPrivateKey(intKey)
	for _, f := range []struct {
		file, typ string
		der       []byte
		mode      os.FileMode
	}{
		{ca.RootKey, "EC PRIVATE KEY", rk, 0o600},
		{ca.IntKey, "EC PRIVATE KEY", ik, 0o600},
		{ca.RootCert, "CERTIFICATE", rootDER, 0o644},
		{ca.IntCert, "CERTIFICATE", intDER, 0o644},
	} {
		if err := writePEM(f.file, f.typ, f.der, f.mode); err != nil {
			return err
		}
	}
	return nil
}

func writeFileAtomicMode(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
