package devserve

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// TestDualListenerAndKubeconfig serves one handler on one port over TLS and
// plain HTTP, and checks that a kubeconfig written by WriteKubeconfig, loaded
// the way the backend loads --kubeconfig, sends its bearer token over TLS.
func TestDualListenerAndKubeconfig(t *testing.T) {
	cert, caPEM, err := SelfSignedCert("127.0.0.1")
	if err != nil {
		t.Fatalf("SelfSignedCert() error = %v", err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := NewDualListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		_, _ = io.WriteString(w, scheme+" "+r.Header.Get("Authorization"))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := l.Addr().String()

	get := func(client *http.Client, url, token string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	if got := get(http.DefaultClient, "http://"+addr+"/", "plain"); got != "http Bearer plain" {
		t.Fatalf("plain HTTP = %q", got)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	if got := get(tlsClient, "https://"+addr+"/", "secure"); got != "https Bearer secure" {
		t.Fatalf("TLS = %q", got)
	}

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if _, err := WriteKubeconfig(path, "https://"+addr, caPEM, "sa-token"); err != nil {
		t.Fatalf("WriteKubeconfig() error = %v", err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("BuildConfigFromFlags() error = %v", err)
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("HTTPClientFor() error = %v", err)
	}
	if got := get(httpClient, cfg.Host+"/", ""); got != "https Bearer sa-token" {
		t.Fatalf("kubeconfig client = %q", got)
	}
}
