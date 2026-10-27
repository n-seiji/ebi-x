package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testAppKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// verifyJWT checks the App JWT's signature and issuer.
func verifyJWT(t *testing.T, header string, key *rsa.PrivateKey, appID string) bool {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(header, "Bearer "), ".")
	if len(parts) != 3 {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return false
	}
	return claims.Iss == appID && claims.Exp-claims.Iat <= 10*60
}

type testApp struct {
	tokens  *AppTokens
	mints   atomic.Int32
	revoked sync.Map
}

func newTestApp(t *testing.T, installations string, installationID int64) *testApp {
	t.Helper()
	key, keyPEM := testAppKey(t)
	app := &testApp{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/installation/token" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !strings.HasPrefix(token, "ghs_") {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			app.revoked.Store(token, true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !verifyJWT(t, r.Header.Get("Authorization"), key, "123") {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = w.Write([]byte(installations))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			n := app.mints.Add(1)
			_, _ = w.Write([]byte(`{"token":"ghs_` + strconv.Itoa(int(n)) + `","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	tokens, err := NewAppTokens(server.URL, "123", installationID, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	tokens.http = server.Client()
	app.tokens = tokens
	return app
}

func TestAppTokensMintsFreshTokens(t *testing.T) {
	app := newTestApp(t, `[{"id":42,"account":{"login":"miive-project"}}]`, 0)
	first, err := app.tokens.Mint(context.Background())
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	second, err := app.tokens.Mint(context.Background())
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	if first != "ghs_1" || second != "ghs_2" {
		t.Fatalf("tokens = %q, %q; want a new token each time", first, second)
	}
	if err := app.tokens.Revoke(context.Background(), first); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if _, ok := app.revoked.Load(first); !ok {
		t.Fatal("token was not revoked")
	}
}

func TestTurnEnvRevokesTokenOnCleanup(t *testing.T) {
	app := newTestApp(t, `[{"id":42,"account":{"login":"miive-project"}}]`, 0)
	env, cleanup, err := app.tokens.TurnEnv(context.Background())
	if err != nil {
		t.Fatalf("TurnEnv() error = %v", err)
	}
	var ghDir string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "GH_CONFIG_DIR="); ok {
			ghDir = value
		}
	}
	if _, err := os.Stat(ghDir); err != nil {
		t.Fatalf("turn gh configuration missing: %v", err)
	}
	cleanup()
	if _, ok := app.revoked.Load("ghs_1"); !ok {
		t.Fatal("turn token was not revoked after the turn")
	}
	if _, err := os.Stat(filepath.Dir(ghDir)); !os.IsNotExist(err) {
		t.Fatalf("turn credential directory still exists: %v", err)
	}
}

func TestAppTokensNeedsInstallationIDForSeveralInstallations(t *testing.T) {
	installations := `[{"id":42,"account":{"login":"a"}},{"id":43,"account":{"login":"b"}}]`
	several := newTestApp(t, installations, 0)
	if _, err := several.tokens.Mint(context.Background()); err == nil || !strings.Contains(err.Error(), "EBIX_GITHUB_APP_INSTALLATION_ID") {
		t.Fatalf("Token() error = %v, want a hint to set the installation ID", err)
	}
	chosen := newTestApp(t, installations, 42)
	if token, err := chosen.tokens.Mint(context.Background()); err != nil || token != "ghs_1" {
		t.Fatalf("Token() = %q, %v; want the chosen installation's token", token, err)
	}
}

func TestNewAppTokensAcceptsPKCS8(t *testing.T) {
	key, _ := testAppKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAppTokens("", "1", 0, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		t.Fatalf("NewAppTokens(PKCS8) error = %v", err)
	}
	if _, err := NewAppTokens("", "1", 0, []byte("not a key")); err == nil {
		t.Fatal("NewAppTokens(garbage) error = nil, want an error")
	}
}
