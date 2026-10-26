package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
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

func testAppTokens(t *testing.T, installations string, installationID int64) (*AppTokens, *atomic.Int32) {
	t.Helper()
	key, keyPEM := testAppKey(t)
	var mints atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !verifyJWT(t, r.Header.Get("Authorization"), key, "123") {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = w.Write([]byte(installations))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			n := mints.Add(1)
			_, _ = w.Write([]byte(`{"token":"ghs_` + string(rune('0'+n)) + `","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
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
	return tokens, &mints
}

func TestAppTokensMintsAndCaches(t *testing.T) {
	tokens, mints := testAppTokens(t, `[{"id":42,"account":{"login":"miive-project"}}]`, 0)
	first, err := tokens.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	second, err := tokens.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if first != "ghs_1" || second != first || mints.Load() != 1 {
		t.Fatalf("tokens = %q, %q after %d mints; want one cached token", first, second, mints.Load())
	}
	// Close to expiry, the next turn gets a fresh token.
	tokens.now = func() time.Time { return time.Now().Add(50 * time.Minute) }
	third, err := tokens.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if third != "ghs_2" {
		t.Fatalf("token near expiry = %q, want a new one", third)
	}
}

func TestAppTokensNeedsInstallationIDForSeveralInstallations(t *testing.T) {
	installations := `[{"id":42,"account":{"login":"a"}},{"id":43,"account":{"login":"b"}}]`
	tokens, _ := testAppTokens(t, installations, 0)
	if _, err := tokens.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "EBIX_GITHUB_APP_INSTALLATION_ID") {
		t.Fatalf("Token() error = %v, want a hint to set the installation ID", err)
	}
	chosen, _ := testAppTokens(t, installations, 42)
	if token, err := chosen.Token(context.Background()); err != nil || token != "ghs_1" {
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

func TestKeychainSecretDecodesHex(t *testing.T) {
	pemKey := "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n"
	if got := string(keychainSecret([]byte(hex.EncodeToString([]byte(pemKey)) + "\n"))); got != pemKey {
		t.Fatalf("keychainSecret(hex) = %q, want the PEM key", got)
	}
	if got := string(keychainSecret([]byte("plain\n"))); got != "plain" {
		t.Fatalf("keychainSecret(plain) = %q, want it unchanged", got)
	}
}
