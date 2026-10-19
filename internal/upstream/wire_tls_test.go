package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWireSendsTheOpenSSLClientHello(t *testing.T) {
	seen := make(chan []uint16, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	base := srv.TLS.Clone()
	srv.TLS = &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case seen <- chi.CipherSuites:
		default:
		}
		return base, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	w := NewWire(testTimeouts(), nil)
	w.Roots = roots
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := w.Do(ctx, Request{Method: "GET", URL: srv.URL + "/x"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	cs := <-seen
	if len(cs) == 0 || cs[0] != 0x1302 || cs[len(cs)-1] != 0x0067 {
		t.Fatalf("ClientHello cipher order: %x", cs)
	}
}
