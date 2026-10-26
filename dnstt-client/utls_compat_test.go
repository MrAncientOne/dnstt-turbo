package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

func localTLSCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

// Test the updated browser TLS profiles, verified certificates, ALPN and HTTP
// codecs over real TLS records on pipes. OS TCP connects are refused in this
// runtime; this does not claim kernel/WAN or browser-fingerprint equivalence.
func TestUTLSVerifiedHTTPCompatibility(t *testing.T) {
	cert, roots := localTLSCertificate(t)
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, h2 := range []bool{false, true} {
			for _, profile := range []string{"Firefox", "Chrome", "Firefox_120", "Chrome_120"} {
				t.Run(fmt.Sprintf("tls_%x/h2_%t/%s", version, h2, profile), func(t *testing.T) {
					a, b := net.Pipe()
					defer a.Close()
					defer b.Close()
					a.SetDeadline(time.Now().Add(3 * time.Second))
					b.SetDeadline(time.Now().Add(3 * time.Second))
					proto := "http/1.1"
					if h2 {
						proto = "h2"
					}
					server := tls.Server(b, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: version, MaxVersion: version, NextProtos: []string{proto}})
					handshake := make(chan error, 1)
					go func() { handshake <- server.Handshake() }()
					client := utls.UClient(a, &utls.Config{RootCAs: roots, ServerName: "localhost"}, *utlsLookup(profile))
					if err := client.Handshake(); err != nil {
						t.Fatal(err)
					}
					if err := <-handshake; err != nil {
						t.Fatal(err)
					}
					if client.ConnectionState().NegotiatedProtocol != proto {
						t.Fatal("unexpected ALPN")
					}
					body := []byte{0, 1, 2, 255}
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						p, err := io.ReadAll(r.Body)
						if err != nil || !bytes.Equal(p, body) {
							w.WriteHeader(400)
							return
						}
						w.Header().Set("Content-Type", "application/dns-message")
						w.Write(p)
					})
					var rt http.RoundTripper
					if h2 {
						go (&http2.Server{}).ServeConn(server, &http2.ServeConnOpts{Handler: handler})
						cc, err := (&http2.Transport{}).NewClientConn(client)
						if err != nil {
							t.Fatal(err)
						}
						defer cc.Close()
						rt = cc
					} else {
						ln := &pipeListener{incoming: make(chan net.Conn, 1), done: make(chan struct{})}
						ln.incoming <- server
						hs := &http.Server{Handler: handler}
						defer hs.Close()
						go hs.Serve(ln)
						tr := &http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) { return client, nil }}
						defer tr.CloseIdleConnections()
						rt = tr
					}
					for n := 0; n < 2; n++ {
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						req, _ := http.NewRequestWithContext(ctx, "POST", "https://localhost/dns-query", bytes.NewReader(body))
						resp, err := rt.RoundTrip(req)
						if err != nil {
							cancel()
							t.Fatal(err)
						}
						got, err := io.ReadAll(resp.Body)
						resp.Body.Close()
						cancel()
						if err != nil || resp.StatusCode != 200 || !bytes.Equal(got, body) {
							t.Fatalf("HTTP response %d %x %v", resp.StatusCode, got, err)
						}
						wantProto := 1
						if h2 {
							wantProto = 2
						}
						if resp.ProtoMajor != wantProto {
							t.Fatalf("ALPN selected %s", resp.Proto)
						}
					}
				})
			}
		}
	}
}

func TestUTLSRejectsUntrustedCertificate(t *testing.T) {
	cert, _ := localTLSCertificate(t)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	a.SetDeadline(time.Now().Add(time.Second))
	b.SetDeadline(time.Now().Add(time.Second))
	go tls.Server(b, &tls.Config{Certificates: []tls.Certificate{cert}}).Handshake()
	c := utls.UClient(a, &utls.Config{RootCAs: x509.NewCertPool(), ServerName: "localhost"}, *utlsLookup("Firefox"))
	err := c.Handshake()
	var untrusted x509.UnknownAuthorityError
	if !errors.As(err, &untrusted) {
		t.Fatalf("expected certificate verification failure, got %v", err)
	}
}
