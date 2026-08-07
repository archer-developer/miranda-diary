// Package tlscert generates and persists a self-signed TLS certificate for
// miranda-diary's HTTPS listener. Traffic between Miranda and the diary
// carries a per-user encryption key (record_encryption_key, see
// internal/diary/crypto.go) on every tool call once encryption is enabled
// for a household member, so the transport needs to be encrypted even though
// today both services run on the same host over loopback.
//
// A full CA-issued certificate is overkill for a link that never leaves
// localhost and has no public DNS name, so the server generates and trusts
// its own self-signed certificate instead.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// validity is deliberately long: this certificate is never presented to a
// browser or checked against a public CA, so there's no revocation or
// rotation infrastructure watching it. Regenerating it would also require
// updating whatever trust store the client (Miranda) pins it in, so
// EnsureSelfSigned avoids doing that as part of routine restarts (see below).
const validity = 10 * 365 * 24 * time.Hour

// EnsureSelfSigned makes sure certPath and keyPath exist and cover hosts,
// generating a new self-signed certificate (a mix of IP addresses and DNS
// names) when either file is missing or the existing certificate's SAN list
// no longer matches hosts. Otherwise the existing files are reused unchanged
// — regenerating on every restart would rotate the public certificate out
// from under the client's pinned trust, breaking the connection until the
// new cert is copied over by hand.
func EnsureSelfSigned(certPath, keyPath string, hosts []string) error {
	certExists, err := statExists(certPath)
	if err != nil {
		return fmt.Errorf("tlscert: stat cert file: %w", err)
	}
	keyExists, err := statExists(keyPath)
	if err != nil {
		return fmt.Errorf("tlscert: stat key file: %w", err)
	}
	if certExists && keyExists {
		covers, err := certCoversHosts(certPath, hosts)
		if err != nil {
			return fmt.Errorf("tlscert: check existing cert: %w", err)
		}
		if covers {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return fmt.Errorf("tlscert: create cert directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return fmt.Errorf("tlscert: create key directory: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("tlscert: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("tlscert: generate serial number: %w", err)
	}

	dnsNames, ips := splitHosts(hosts)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "miranda-diary"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Self-signed: this certificate is its own trust anchor, so it must
		// be a valid CA/root for clients to accept it at all.
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("tlscert: create certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("tlscert: marshal private key: %w", err)
	}

	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return fmt.Errorf("tlscert: write cert: %w", err)
	}
	// 0600: unlike the certificate, the private key must not be readable by
	// other local users.
	if err := writePEM(keyPath, "PRIVATE KEY", keyDER, 0o600); err != nil {
		return fmt.Errorf("tlscert: write key: %w", err)
	}
	return nil
}

// statExists reports whether path exists, distinguishing "does not exist"
// from a real stat error (permissions, I/O) — the latter is propagated
// rather than treated as "no certificate yet," so EnsureSelfSigned doesn't
// silently regenerate over an existing cert it just couldn't check.
func statExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// splitHosts partitions hosts into IP addresses and DNS names for use as a
// certificate's Subject Alternative Names.
func splitHosts(hosts []string) (dnsNames []string, ips []net.IP) {
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, host)
		}
	}
	return dnsNames, ips
}

// certCoversHosts reports whether the certificate at certPath's SAN list
// (as a set, order-independent) exactly matches hosts. A read/parse failure
// on the existing file is returned as an error rather than treated as "no
// match" — a corrupt or unreadable cert should fail startup loudly, not be
// silently overwritten.
func certCoversHosts(certPath string, hosts []string) (bool, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return false, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false, fmt.Errorf("tlscert: no PEM block in %s", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, err
	}

	wantDNS, wantIPs := splitHosts(hosts)
	gotDNS := slices.Clone(cert.DNSNames)
	slices.Sort(gotDNS)
	slices.Sort(wantDNS)
	if !slices.Equal(gotDNS, wantDNS) {
		return false, nil
	}

	gotIPs := make([]string, len(cert.IPAddresses))
	for i, ip := range cert.IPAddresses {
		gotIPs[i] = ip.String()
	}
	wantIPStrs := make([]string, len(wantIPs))
	for i, ip := range wantIPs {
		wantIPStrs[i] = ip.String()
	}
	slices.Sort(gotIPs)
	slices.Sort(wantIPStrs)
	return slices.Equal(gotIPs, wantIPStrs), nil
}

func writePEM(path, blockType string, der []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}
