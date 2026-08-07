package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureSelfSigned_GeneratesUsableCert verifies the generated
// certificate is actually acceptable to a Go TLS client that trusts it —
// not just well-formed PEM — and that the client rejects it once trust is
// removed, so a genuine MITM without the cert would fail the same way.
func TestEnsureSelfSigned_GeneratesUsableCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	err := EnsureSelfSigned(certPath, keyPath, []string{"127.0.0.1", "localhost"})
	require.NoError(t, err)

	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	keyPEM, err := os.ReadFile(keyPath)
	require.NoError(t, err)

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	defer ts.Close()

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM))

	trusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := trusting.Get(ts.URL)
	require.NoError(t, err, "client trusting the generated cert should succeed")
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	untrusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}}}
	_, err = untrusting.Get(ts.URL)
	require.Error(t, err, "client without the cert in its trust store should fail verification")
}

// TestEnsureSelfSigned_Reuses verifies a second call does not rotate the
// certificate — regenerating on every restart would invalidate whatever
// trust store the client has pinned it in.
func TestEnsureSelfSigned_Reuses(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	require.NoError(t, EnsureSelfSigned(certPath, keyPath, []string{"127.0.0.1"}))
	firstCert, err := os.ReadFile(certPath)
	require.NoError(t, err)

	require.NoError(t, EnsureSelfSigned(certPath, keyPath, []string{"127.0.0.1"}))
	secondCert, err := os.ReadFile(certPath)
	require.NoError(t, err)

	require.Equal(t, firstCert, secondCert)
}

// TestEnsureSelfSigned_CoversHosts verifies both IP and DNS name entries in
// hosts land in the right x509 SAN field — an IP string that's silently
// absorbed as a DNS name wouldn't validate against a client dialing that IP.
func TestEnsureSelfSigned_CoversHosts(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	require.NoError(t, EnsureSelfSigned(certPath, keyPath, []string{"127.0.0.1", "localhost"}))

	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	require.Contains(t, cert.DNSNames, "localhost")
	require.Len(t, cert.IPAddresses, 1)
	require.True(t, cert.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")))
}

// TestEnsureSelfSigned_RegeneratesWhenHostsChange verifies that reuse is
// gated on more than file presence: adding a host to the configured list
// after the certificate already exists must trigger regeneration, or TLS
// handshakes against the new host would fail with no indication why.
func TestEnsureSelfSigned_RegeneratesWhenHostsChange(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	require.NoError(t, EnsureSelfSigned(certPath, keyPath, []string{"127.0.0.1"}))
	firstCert, err := os.ReadFile(certPath)
	require.NoError(t, err)

	require.NoError(t, EnsureSelfSigned(certPath, keyPath, []string{"127.0.0.1", "localhost"}))
	secondCert, err := os.ReadFile(certPath)
	require.NoError(t, err)

	require.NotEqual(t, firstCert, secondCert, "cert should be regenerated when the host list gains an entry")

	block, _ := pem.Decode(secondCert)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.Contains(t, cert.DNSNames, "localhost")
	require.Len(t, cert.IPAddresses, 1)
	require.True(t, cert.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")))
}
