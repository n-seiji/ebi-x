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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// KeychainPrefix marks a private key location as a macOS Keychain item: the
// rest is the item's service name.
const KeychainPrefix = "keychain:"

// tokenRefreshMargin is how long before expiry a cached installation token
// is replaced, so a turn never starts with a token about to lapse. GitHub
// issues tokens for an hour.
const tokenRefreshMargin = 15 * time.Minute

// AppTokens mints installation tokens of a GitHub App. Only the bot holds
// the App's private key; work turns get the short-lived tokens.
type AppTokens struct {
	apiURL  string
	webHost string
	appID   string
	key     *rsa.PrivateKey
	http    *http.Client
	now     func() time.Time

	mu             sync.Mutex
	installationID int64
	token          string
	expires        time.Time
}

// NewAppTokens returns a token source for the App appID with the PEM private
// key keyPEM, on apiURL or github.com when it is empty. installationID 0
// means the App's only installation.
func NewAppTokens(apiURL, appID string, installationID int64, keyPEM []byte) (*AppTokens, error) {
	client, err := NewClient(apiURL, "")
	if err != nil {
		return nil, err
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	return &AppTokens{
		apiURL:         client.apiURL,
		webHost:        client.webHost,
		appID:          appID,
		key:            key,
		http:           &http.Client{Timeout: 30 * time.Second},
		now:            time.Now,
		installationID: installationID,
	}, nil
}

// WebHost is the GitHub host git and gh reach with the tokens.
func (a *AppTokens) WebHost() string {
	return a.webHost
}

// Token returns an installation token valid for at least tokenRefreshMargin.
func (a *AppTokens) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && a.now().Add(tokenRefreshMargin).Before(a.expires) {
		return a.token, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	if a.installationID == 0 {
		id, err := a.onlyInstallation(ctx, jwt)
		if err != nil {
			return "", err
		}
		a.installationID = id
	}
	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(a.installationID, 10) + "/access_tokens"
	if err := a.call(ctx, http.MethodPost, path, jwt, &minted); err != nil {
		return "", fmt.Errorf("mint GitHub App installation token: %w", err)
	}
	if minted.Token == "" {
		return "", errors.New("mint GitHub App installation token: empty token")
	}
	a.token, a.expires = minted.Token, minted.ExpiresAt
	return a.token, nil
}

// onlyInstallation returns the App's installation when it has exactly one.
func (a *AppTokens) onlyInstallation(ctx context.Context, jwt string) (int64, error) {
	var installations []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := a.call(ctx, http.MethodGet, "/app/installations?per_page=100", jwt, &installations); err != nil {
		return 0, fmt.Errorf("list GitHub App installations: %w", err)
	}
	if len(installations) != 1 {
		var accounts []string
		for _, installation := range installations {
			accounts = append(accounts, installation.Account.Login+"="+strconv.FormatInt(installation.ID, 10))
		}
		return 0, fmt.Errorf("GitHub App has %d installations %v; set EBIX_GITHUB_APP_INSTALLATION_ID", len(installations), accounts)
	}
	return installations[0].ID, nil
}

// jwt returns the App's JSON Web Token, which GitHub accepts for up to ten
// minutes. iat is backdated to allow for clock drift.
func (a *AppTokens) jwt() (string, error) {
	now := a.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	if err != nil {
		return "", err
	}
	signed := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (a *AppTokens) call(ctx context.Context, method, path, jwt string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func parsePrivateKey(keyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("GitHub App private key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App private key is not an RSA key")
	}
	return key, nil
}

// LoadPrivateKey reads the PEM private key at location: a file path, or
// KeychainPrefix followed by a macOS Keychain service name.
func LoadPrivateKey(location string) ([]byte, error) {
	if service, keychain := strings.CutPrefix(location, KeychainPrefix); keychain {
		return readKeychain(service)
	}
	return os.ReadFile(location)
}
