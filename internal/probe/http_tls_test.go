package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func tlsServer(t *testing.T, notAfter time.Time) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{"127.0.0.1", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPSProbeCapturesCertificate(t *testing.T) {
	notAfter := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	srv := tlsServer(t, notAfter)
	p := NewHTTPWithClient(srv.Client())
	res := p.Probe(context.Background(), checkFor(t, srv.URL, ""))
	if res.Outcome != "up" {
		t.Fatalf("outcome = %q (%s)", res.Outcome, res.Reason())
	}
	if res.CertNotAfter.IsZero() {
		t.Fatal("https probe did not capture the leaf certificate")
	}
	if !res.CertNotAfter.Equal(notAfter) {
		t.Errorf("CertNotAfter = %s, want %s", res.CertNotAfter, notAfter)
	}
}

func TestHTTPSProbeCapturesExpiredCertificate(t *testing.T) {
	notAfter := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	srv := tlsServer(t, notAfter)
	p := NewHTTPWithClient(srv.Client())
	res := p.Probe(context.Background(), checkFor(t, srv.URL, ""))
	if res.Outcome != "down" {
		t.Fatalf("expired cert must fail the probe, got %q", res.Outcome)
	}
	if res.CertNotAfter.IsZero() {
		t.Fatal("expired handshake still has to expose NotAfter, or the expiry alert is silent")
	}
	if !res.CertNotAfter.Equal(notAfter) {
		t.Errorf("CertNotAfter = %s, want %s", res.CertNotAfter, notAfter)
	}
}

func TestHTTPProbeHasNoCertificate(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	res := NewHTTP().Probe(context.Background(), checkFor(t, srv.URL, ""))
	if !res.CertNotAfter.IsZero() {
		t.Errorf("plain http must not invent a certificate: %s", res.CertNotAfter)
	}
}
