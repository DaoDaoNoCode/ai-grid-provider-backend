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
// then stalls in the body must not hold up other requests.
func TestStalledBodyDoesNotBlockOthers(t *testing.T) {
	f := start(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: kc\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"name\":", orgs, f.token); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the server start reading the body

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
