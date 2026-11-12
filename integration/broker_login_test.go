package integration

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

type brokerIdentity struct {
	Issuer            string
	Subject           string
	LocalUsername     string
	Email             string
	PreferredUsername string
	Organization      string
	Groups            []string
	Roles             []string
	ProviderAlias     string
	ProviderSubject   string
}

type brokerLoginError struct {
	Operation string
	Status    int
	Body      string
}

type brokerLoginOptions struct {
	expectConflict         bool
	forgedUsername         string
	expectProfileRejection bool
}

func (e *brokerLoginError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("broker login %s: status %d", e.Operation, e.Status)
	}
	return fmt.Sprintf("broker login %s: status %d: %s", e.Operation, e.Status, e.Body)
}

type adminUser struct {
	ID                  string              `json:"id"`
	Username            string              `json:"username"`
	Email               string              `json:"email"`
	FirstName           string              `json:"firstName"`
	LastName            string              `json:"lastName"`
	Attributes          map[string][]string `json:"attributes"`
	FederatedIdentities []struct {
		IdentityProvider string `json:"identityProvider"`
		UserID           string `json:"userId"`
		UserName         string `json:"userName"`
	} `json:"federatedIdentities"`
}

var brokerLoginAdmin adminContext
var brokerLoginCallbackPort int

type adminContext struct {
	BaseURL    string
	AdminRealm string
	Realm      string
	Username   string
	Password   string
	Token      string
	HTTPClient *http.Client
}

func (admin adminContext) request(t *testing.T, method, endpoint string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		t.Fatalf("create %s request: %v", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("%s request: %v", method, err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	_ = resp.Body.Close()
	return resp, responseBody
}

func loginViaBroker(t *testing.T, issuerURL, clientID, providerAlias, username, password string) brokerIdentity {
	t.Helper()
	return loginViaBrokerWithOptions(t, issuerURL, clientID, providerAlias, username, password, brokerLoginOptions{})
}

func loginViaBrokerExpectConflict(t *testing.T, issuerURL, clientID, providerAlias, username, password string) brokerIdentity {
	t.Helper()
	return loginViaBrokerWithOptions(t, issuerURL, clientID, providerAlias, username, password, brokerLoginOptions{expectConflict: true})
}

func loginViaBrokerWithForgedUsername(t *testing.T, issuerURL, clientID, providerAlias, username, password string) brokerIdentity {
	t.Helper()
	return loginViaBrokerWithOptions(t, issuerURL, clientID, providerAlias, username, password, brokerLoginOptions{
		forgedUsername:         "forged-local-username",
		expectProfileRejection: true,
	})
}

func loginViaBrokerWithOptions(t *testing.T, issuerURL, clientID, providerAlias, username, password string, options brokerLoginOptions) brokerIdentity {
	t.Helper()
	if issuerURL == "" || clientID == "" || providerAlias == "" || username == "" || password == "" {
		t.Fatal("loginViaBroker requires issuerURL, clientID, providerAlias, username, and password")
	}
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback", brokerLoginCallbackPort)
	state := randomHex(t, 16)
	nonce := randomHex(t, 16)
	authURL := buildAuthorizationURL(issuerURL, clientID, callbackURL, state, nonce, providerAlias)

	codeCh := make(chan string, 1)
	server := startCallbackServer(t, brokerLoginCallbackPort, codeCh)
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
	}

	resp, body := doGet(t, client, authURL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorization request status = %d, want 200: %s", resp.StatusCode, body)
	}
	formAction, values := parseLoginForm(t, body)
	if _, ok := values["password"]; !ok {
		t.Fatalf("initial login form has no password field: %s", body)
	}
	values["username"] = username
	values["password"] = password

	forgedSubmitted := false
	for i := 0; i < 5; i++ {
		resp, body = doPostForm(t, client, formAction, values)
		select {
		case code := <-codeCh:
			if options.expectProfileRejection {
				if !forgedSubmitted {
					t.Fatal("broker login reached the callback before the forged username was submitted")
				}
				t.Fatal("forged local username was accepted")
			}
			_ = resp.Body.Close()
			return exchangeCodeAndBuildIdentity(t, client, issuerURL, clientID, callbackURL, code, providerAlias)
		default:
		}
		if resp.StatusCode != http.StatusOK {
			if options.expectConflict && resp.StatusCode == http.StatusConflict {
				return brokerIdentity{}
			}
			t.Fatalf("broker login status = %d, want 200", resp.StatusCode)
		}
		formAction, values = parseLoginForm(t, body)
		if options.expectProfileRejection && formActionBelongsToRealm(formAction, issuerURL) {
			values["username"] = options.forgedUsername
			forgedSubmitted = true
			resp, body = doPostForm(t, client, formAction, values)
			select {
			case <-codeCh:
				if !forgedSubmitted {
					t.Fatal("broker login reached the callback before the forged username was submitted")
				}
				t.Fatal("forged local username was accepted")
			default:
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("forged username profile response status = %d, want 200: %s", resp.StatusCode, body)
			}
			return brokerIdentity{}
		}
		if _, ok := values["password"]; ok {
			t.Fatalf("unexpected second password form: %s", body)
		}
		if values["firstName"] == "" {
			values["firstName"] = "Test"
		}
		if values["lastName"] == "" {
			values["lastName"] = "User"
		}
		if values["email"] == "" {
			values["email"] = providerAlias + "@example.com"
		}
	}
	if options.expectProfileRejection {
		t.Fatalf("broker login did not reach a target-realm profile form: %s", body)
	}
	t.Fatalf("broker login did not reach the callback after five forms: %s", body)
	return brokerIdentity{}
}

func formActionBelongsToRealm(formAction, issuerURL string) bool {
	base, err := url.Parse(issuerURL)
	if err != nil {
		return false
	}
	action, err := url.Parse(formAction)
	if err != nil {
		return false
	}
	resolved := base.ResolveReference(action)
	return strings.EqualFold(resolved.Host, base.Host) && strings.HasPrefix(resolved.Path, strings.TrimRight(base.Path, "/")+"/")
}

func buildAuthorizationURL(issuerURL, clientID, callbackURL, state, nonce, providerAlias string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", callbackURL)
	q.Set("response_type", "code")
	q.Set("scope", "openid profile email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("kc_idp_hint", providerAlias)
	return strings.TrimRight(issuerURL, "/") + "/protocol/openid-connect/auth?" + q.Encode()
}

func startCallbackServer(t *testing.T, port int, codeCh chan<- string) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code != "" {
			select {
			case codeCh <- code:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("callback complete"))
	})
	server := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatalf("listen on callback server: %v", err)
	}
	go func() {
		_ = server.Serve(listener)
	}()
	return server
}

func doGet(t *testing.T, client *http.Client, target string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response from %s: %v", target, err)
	}
	return resp, string(body)
}

func doPostForm(t *testing.T, client *http.Client, action string, values map[string]string) (*http.Response, string) {
	t.Helper()
	form := url.Values{}
	for key, value := range values {
		form.Set(key, value)
	}
	resp, err := client.PostForm(action, form)
	if err != nil {
		t.Fatalf("POST form to %s: %v", action, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response from %s: %v", action, err)
	}
	return resp, string(body)
}

func parseLoginForm(t *testing.T, body string) (string, map[string]string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse login form: %v", err)
	}
	var action string
	values := map[string]string{}
	var findForm func(n *html.Node)
	var findInputs func(n *html.Node)
	findForm = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "form" {
			for _, attr := range n.Attr {
				if attr.Key == "action" {
					action = attr.Val
				}
			}
			findInputs(n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findForm(c)
		}
	}
	findInputs = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "input" {
			name, value := "", ""
			for _, attr := range n.Attr {
				switch attr.Key {
				case "name":
					name = attr.Val
				case "value":
					value = attr.Val
				}
			}
			if name != "" {
				values[name] = value
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findInputs(c)
		}
	}
	findForm(doc)
	if action == "" {
		t.Fatalf("login form has no action: %s", body)
	}
	return action, values
}

func exchangeCodeAndBuildIdentity(t *testing.T, client *http.Client, issuerURL, clientID, callbackURL, code, providerAlias string) brokerIdentity {
	t.Helper()
	tokenURL := strings.TrimRight(issuerURL, "/") + "/protocol/openid-connect/token"
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", callbackURL)
	form.Set("code", code)
	resp, err := client.PostForm(tokenURL, form)
	if err != nil {
		t.Fatalf("exchange authorization code: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read token response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token exchange status = %d, want 200", resp.StatusCode)
	}
	var token struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if token.IDToken == "" {
		t.Fatal("token response missing id_token")
	}
	claims, err := parseJWTClaims(token.IDToken)
	if err != nil {
		t.Fatalf("parse ID token claims: %v", err)
	}
	email := stringClaim(claims, "email")
	identity := brokerIdentity{
		Issuer:        stringClaim(claims, "iss"),
		Subject:       stringClaim(claims, "sub"),
		Organization:  stringClaim(claims, "org_id"),
		Email:         email,
		Groups:        stringSliceClaim(claims, "groups"),
		Roles:         stringSliceClaim(claims, "roles"),
		ProviderAlias: providerAlias,
	}
	if preferred, ok := claims["preferred_username"].(string); ok {
		identity.PreferredUsername = preferred
	}
	user, err := adminUserByEmail(t, email)
	if err != nil {
		t.Fatalf("read broker user by email %q: %v", email, err)
	}
	identity.LocalUsername = user.Username
	for _, link := range user.FederatedIdentities {
		if link.IdentityProvider == providerAlias {
			identity.ProviderSubject = link.UserID
			break
		}
	}
	return identity
}

func parseJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("ID token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode ID token payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("decode ID token claims: %w", err)
	}
	return claims, nil
}

func stringClaim(claims map[string]any, key string) string {
	if value, ok := claims[key].(string); ok {
		return value
	}
	return ""
}

func stringSliceClaim(claims map[string]any, key string) []string {
	values, ok := claims[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if s, ok := value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func adminUserByEmail(t *testing.T, email string) (adminUser, error) {
	t.Helper()
	if email == "" {
		return adminUser{}, errors.New("email claim is empty")
	}
	endpoint := fmt.Sprintf("%s/admin/realms/%s/users?email=%s&exact=true&briefRepresentation=false",
		brokerLoginAdmin.BaseURL, url.PathEscape(brokerLoginAdmin.Realm), url.QueryEscape(email))
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return adminUser{}, fmt.Errorf("create user search request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+brokerLoginAdmin.Token)
	resp, err := brokerLoginAdmin.HTTPClient.Do(req)
	if err != nil {
		return adminUser{}, fmt.Errorf("request user search: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return adminUser{}, fmt.Errorf("read user search response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return adminUser{}, &brokerLoginError{Operation: "search user", Status: resp.StatusCode, Body: string(body)}
	}
	var users []adminUser
	if err := json.Unmarshal(body, &users); err != nil {
		return adminUser{}, fmt.Errorf("decode user search response: %w", err)
	}
	if len(users) != 1 {
		return adminUser{}, fmt.Errorf("user search returned %d users, want 1", len(users))
	}
	return getAdminUser(t, brokerLoginAdmin, brokerLoginAdmin.Realm, users[0].ID), nil
}

func randomHex(t *testing.T, length int) string {
	t.Helper()
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random value: %v", err)
	}
	return hex.EncodeToString(b)
}
