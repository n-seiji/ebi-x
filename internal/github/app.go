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

const KeychainPrefix = "keychain:"

// Only the bot holds the App's private key; work turns get the short-lived
// tokens.
type AppTokens struct {
	apiURL      string
	webHost     string
	dir         string
	appID       string
	key         *rsa.PrivateKey
	http        *http.Client
	now         func() time.Time
	mintBackoff [mintAttempts - 1]time.Duration

	mu             sync.Mutex
	installationID int64
}

func NewAppTokens(apiURL, appID string, installationID int64, keyPEM []byte, turnDir string) (*AppTokens, error) {
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
		dir:            turnDir,
		appID:          appID,
		key:            key,
		http:           &http.Client{Timeout: 30 * time.Second},
		now:            time.Now,
		mintBackoff:    [mintAttempts - 1]time.Duration{mintFirstBackoff, mintSecondBackoff},
		installationID: installationID,
	}, nil
}

func (a *AppTokens) WebHost() string {
	return a.webHost
}

func (a *AppTokens) Mint(ctx context.Context) (string, error) {
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	installationID, err := a.installation(ctx, jwt)
	if err != nil {
		return "", err
	}
	var minted struct {
		Token string `json:"token"`
	}
	path := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	if err := a.call(ctx, http.MethodPost, path, jwt, &minted); err != nil {
		return "", fmt.Errorf("mint GitHub App installation token: %w", err)
	}
	if minted.Token == "" {
		return "", errors.New("mint GitHub App installation token: empty token")
	}
	return minted.Token, nil
}

func (a *AppTokens) Revoke(ctx context.Context, token string) error {
	if err := a.call(ctx, http.MethodDelete, "/installation/token", token, nil); err != nil {
		return fmt.Errorf("revoke GitHub App installation token: %w", err)
	}
	return nil
}

func (a *AppTokens) installation(ctx context.Context, jwt string) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.installationID == 0 {
		id, err := a.onlyInstallation(ctx, jwt)
		if err != nil {
			return 0, err
		}
		a.installationID = id
	}
	return a.installationID, nil
}

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
	if len(installations) == 0 {
		return 0, errors.New("GitHub App has no installations; install it on an account")
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

// iat is backdated to allow for clock drift.
func (a *AppTokens) jwt() (string, error) {
	now := a.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	if err != nil {
		return "", fmt.Errorf("encode GitHub App JWT claims: %w", err)
	}
	signed := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (a *AppTokens) call(ctx context.Context, method, path, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &transportError{err: err}
	}
	if resp.StatusCode/100 != 2 {
		return &statusError{method: method, path: path, status: resp.Status, code: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

type statusError struct {
	method, path, status, body string
	code                       int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: %s: %s", e.method, e.path, e.status, e.body)
}

type transportError struct {
	err error
}

func (e *transportError) Error() string { return e.err.Error() }

func (e *transportError) Unwrap() error { return e.err }

// Client errors such as a bad key or a wrong installation fail the same way
// again, so they are not retried.
func isTransient(err error) bool {
	var status *statusError
	if errors.As(err, &status) {
		switch {
		case status.code >= 500, status.code == http.StatusTooManyRequests:
			return true
		case status.code == http.StatusForbidden:
			// GitHub reports a secondary rate limit as 403, like a missing
			// permission, which retrying does not fix.
			return strings.Contains(strings.ToLower(status.body), "rate limit")
		}
		return false
	}
	var transport *transportError
	return errors.As(err, &transport)
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

func LoadPrivateKey(location string) ([]byte, error) {
	if service, keychain := strings.CutPrefix(location, KeychainPrefix); keychain {
		return readKeychain(service)
	}
	key, err := os.ReadFile(location)
	if err != nil {
		return nil, fmt.Errorf("read GitHub App private key: %w", err)
	}
	return key, nil
}
