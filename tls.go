package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Browsers only accept https://yt with a certificate for "yt", and no public
// CA issues those, so hop runs a small local CA that install.sh trusts.

const (
	caCommonName = "hop local CA"
	caCertFile   = "ca.pem"
	caKeyFile    = "ca-key.pem"
	// Apple rejects TLS certificates valid for more than 825 days; a year is
	// well inside that, and leaves are reissued on demand.
	leafLifetime = 365 * 24 * time.Hour
	leafRenewal  = 30 * 24 * time.Hour
)

// certIssuer signs a certificate per host name on first use. allowed limits
// it to names hop serves, so the trusted CA never signs for a real website.
type certIssuer struct {
	caCert  *x509.Certificate
	caKey   crypto.Signer
	allowed func(name string) bool
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

func newCertIssuer(caCert *x509.Certificate, caKey crypto.Signer, allowed func(string) bool) *certIssuer {
	return &certIssuer{caCert: caCert, caKey: caKey, allowed: allowed, now: time.Now, cache: map[string]*tls.Certificate{}}
}

// tlsConfig is the server config for hop's HTTPS listener.
func (ci *certIssuer) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: ci.certificate}
}

func (ci *certIssuer) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	// Clients send no server name when connecting by IP, as the CLI does.
	name := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")
	if name == "" {
		name = "hop"
	}
	if !ci.allowed(strings.TrimSuffix(name, ".localhost")) {
		return nil, fmt.Errorf("no shortcut named %q", name)
	}
	ci.mu.Lock()
	defer ci.mu.Unlock()
	if c, ok := ci.cache[name]; ok && ci.now().Add(leafRenewal).Before(c.Leaf.NotAfter) {
		return c, nil
	}
	c, err := ci.issue(name)
	if err != nil {
		return nil, err
	}
	ci.cache[name] = c
	return c, nil
}

func (ci *certIssuer) issue(name string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key for %s: %w", name, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := ci.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     dnsNamesFor(name),
		NotBefore:    now.Add(-time.Hour), // tolerate a slightly slow client clock
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ci.caCert, &key.PublicKey, ci.caKey)
	if err != nil {
		return nil, fmt.Errorf("sign certificate for %s: %w", name, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate for %s: %w", name, err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// dnsNamesFor covers both ways a name is typed: yt and yt.localhost.
func dnsNamesFor(name string) []string {
	base := strings.TrimSuffix(name, ".localhost")
	return []string{base, base + ".localhost"}
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

// loadOrCreateCA reads the CA from dir, creating it on first run. The key is
// written 0600; install.sh puts dir somewhere only root can read, because
// anyone holding a trusted CA key can impersonate any website.
func loadOrCreateCA(dir string) (*x509.Certificate, crypto.Signer, error) {
	certPath, keyPath := filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
	cert, key, err := readCA(certPath, keyPath)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return cert, key, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create %s: %w", dir, err)
	}
	certPEM, keyPEM, err := newCA()
	if err != nil {
		return nil, nil, err
	}
	// Key first: a cert without its key would block every later start.
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write CA key: %w", err)
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("write CA certificate: %w", err)
	}
	return readCA(certPath, keyPath)
}

func readCA(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load CA from %s: %w", filepath.Dir(certPath), err)
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("CA key in %s cannot sign", keyPath)
	}
	return pair.Leaf, key, nil
}

func newCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode CA key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
