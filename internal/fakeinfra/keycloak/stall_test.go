package keycloak

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestStalledBodyDoesNotBlockOthers: a client that sends its headers and
// then stalls in the body must not hold up other requests. Body bytes are
// read before the server mutex, so concurrent list/token calls succeed
// without waiting on the stalled connection. The server signals
// s.readStarted once it begins the body read, so the GET and token requests
// below only fire once the stalled POST is provably blocked in that read —
// a regression that held the mutex during the read would still fail.
func TestStalledBodyDoesNotBlockOthers(t *testing.T) {
	readStarted := make(chan struct{}, 1)
	f := start(t, func(s *Server) { s.readStarted = readStarted })
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: kc\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"name\":", orgs, f.token); err != nil {
		t.Fatal(err)
	}

	select {
	case <-readStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("server never started reading the stalled body")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+orgs, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("list while another request stalls: %v", err)
	}
	_ = resp.Body.Close()
	resp, err = client.PostForm(f.srv.URL+"/realms/master/protocol/openid-connect/token", url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"secret"}})
	if err != nil {
		t.Fatalf("token while another request stalls: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token = %d", resp.StatusCode)
	}
}
