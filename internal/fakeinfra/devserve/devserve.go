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
	"bytes"
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

// Names in the kubeconfig ai-grid-fake-infra writes. FakeContext as the
// current context is how a fake-infra kubeconfig is recognised: nothing
// else is ever replaced or removed.
const (
	FakeCluster = "ai-grid-fake-hub"
	FakeContext = "ai-grid-fake-hub"
	FakeUser    = "ai-grid-fake-hub-backend"
)

// IsFakeKubeconfig reports whether path holds a kubeconfig WriteKubeconfig
// wrote: exactly one cluster, user and context, with the fake-infra names,
// and FakeContext current. A file that does not parse is not one.
func IsFakeKubeconfig(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return false, nil
	}
	_, cluster := cfg.Clusters[FakeCluster]
	_, user := cfg.AuthInfos[FakeUser]
	_, context := cfg.Contexts[FakeContext]
	return cfg.CurrentContext == FakeContext && cluster && user && context &&
		len(cfg.Clusters) == 1 && len(cfg.AuthInfos) == 1 && len(cfg.Contexts) == 1, nil
}

// WriteKubeconfig writes a kubeconfig for server that trusts caPEM and
// authenticates with token, and returns the bytes it wrote. It refuses to
// replace an existing file that is not a fake-infra kubeconfig, so pointing
// it at a real kubeconfig loses nothing. It writes a temporary file in the
// same directory and renames it, so a reader never sees half a file; the
// mode is 0600.
func WriteKubeconfig(path, server string, caPEM []byte, token string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("kubeconfig path is empty")
	}
	if _, err := os.Lstat(path); err == nil {
		ours, err := IsFakeKubeconfig(path)
		if err != nil {
			return nil, fmt.Errorf("check existing kubeconfig %s: %w", path, err)
		}
		if !ours {
			return nil, fmt.Errorf("%s exists and is not a kubeconfig ai-grid-fake-infra wrote (current-context %s); refusing to replace it, choose another path", path, FakeContext)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("check existing kubeconfig %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create kubeconfig directory: %w", err)
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[FakeCluster] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: caPEM}
	cfg.AuthInfos[FakeUser] = &clientcmdapi.AuthInfo{Token: token}
	cfg.Contexts[FakeContext] = &clientcmdapi.Context{Cluster: FakeCluster, AuthInfo: FakeUser}
	cfg.CurrentContext = FakeContext
	content, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("encode kubeconfig: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return nil, fmt.Errorf("write kubeconfig: %w", err)
	}
	_, werr := tmp.Write(content)
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("write kubeconfig: %w", errors.Join(werr, cerr))
	}
	return content, nil
}

// RemoveKubeconfig removes path only while it still holds exactly the bytes
// WriteKubeconfig returned (written); a file replaced since, by a newer run
// or by anyone else, is left alone. A missing file is not an error.
func RemoveKubeconfig(path string, written []byte) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, written) {
		return nil
	}
	return os.Remove(path)
}
