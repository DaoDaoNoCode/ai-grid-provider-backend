// Package devserve holds what cmd/ai-grid-fake-infra needs to put the fake
// hub where the backend's --kubeconfig can reach it: a self-signed serving
// certificate, a listener that serves TLS and plain HTTP on one port, and the
// kubeconfig itself.
//
// TLS is not optional for the backend: client-go's clientcmd applies a
// kubeconfig user's credentials only when the server is TLS
// (tools/clientcmd/client_config.go, "only try to read the auth information
// if we are secure"), so against a plain http:// server the backend would
// send no bearer token at all. Plain HTTP stays available on the same port
// for the console BFF, whose fake setup addresses the hub as
// http://127.0.0.1:9091. A real API server serves TLS only.
package devserve

import (
	"bufio"
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
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// SelfSignedCert returns a TLS certificate valid for localhost, 127.0.0.1,
// ::1 and host (a name or an IP), and its PEM encoding, which serves as the
// CA a client trusts. It is generated per process and never written with its
// key.
func SelfSignedCert(host string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ai-grid-fake-hub"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsUnspecified() {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	} else if host != "" && host != "localhost" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return cert, certPEM, nil
}

// peekTimeout bounds how long a new connection may stay silent before the
// listener gives up deciding whether it speaks TLS.
const peekTimeout = 10 * time.Second

// DualListener accepts connections on one port and hands each to the HTTP
// server either as TLS (its first byte is a TLS handshake record, 0x16) or as
// plain HTTP.
type DualListener struct {
	inner  net.Listener
	config *tls.Config
	conns  chan net.Conn
	errs   chan error
	done   chan struct{}
	once   sync.Once
}

// NewDualListener wraps inner. config must carry the serving certificate.
func NewDualListener(inner net.Listener, config *tls.Config) *DualListener {
	l := &DualListener{inner: inner, config: config, conns: make(chan net.Conn), errs: make(chan error, 1), done: make(chan struct{})}
	go l.acceptLoop()
	return l
}

func (l *DualListener) acceptLoop() {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			select {
			case l.errs <- err:
			case <-l.done:
			}
			return
		}
		go l.classify(conn)
	}
}

func (l *DualListener) classify(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(peekTimeout))
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var out net.Conn = &peekedConn{Conn: conn, reader: reader}
	if first[0] == 0x16 {
		out = tls.Server(out, l.config)
	}
	select {
	case l.conns <- out:
	case <-l.done:
		_ = conn.Close()
	}
}

// Accept returns the next classified connection.
func (l *DualListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting.
func (l *DualListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		err = l.inner.Close()
	})
	return err
}

// Addr is the listening address.
func (l *DualListener) Addr() net.Addr { return l.inner.Addr() }

// peekedConn reads through the buffered reader that holds the peeked byte.
type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// WriteKubeconfig writes a kubeconfig for server that trusts caPEM and
// authenticates with token. It writes a temporary file and renames it, so a
// reader never sees half a file; the file mode is 0600 (clientcmd.WriteToFile).
func WriteKubeconfig(path, server string, caPEM []byte, token string) error {
	if path == "" {
		return errors.New("kubeconfig path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create kubeconfig directory: %w", err)
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["fake-hub"] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: caPEM}
	cfg.AuthInfos["ai-grid-provider-backend"] = &clientcmdapi.AuthInfo{Token: token}
	cfg.Contexts["fake-hub"] = &clientcmdapi.Context{Cluster: "fake-hub", AuthInfo: "ai-grid-provider-backend"}
	cfg.CurrentContext = "fake-hub"
	tmp := path + ".tmp"
	if err := clientcmd.WriteToFile(*cfg, tmp); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	return nil
}
