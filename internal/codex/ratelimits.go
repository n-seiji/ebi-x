package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxAppServerLineBytes = 1 << 20

var ErrUsageLimit = errors.New("codex usage limit reached")

func IsUsageLimitMessage(message string) bool {
	return strings.Contains(strings.ToLower(message), "usage limit")
}

var ErrNotSignedIn = errors.New("codex is not signed in")

// notSignedInPhrases are how Codex words a missing or expired sign-in, as in
// "Your access token could not be refreshed... Please log out and sign in
// again." or "401 Unauthorized".
var notSignedInPhrases = []string{"sign in again", "not logged in", "401 unauthorized", "access token could not be refreshed"}

func IsNotSignedInMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, phrase := range notSignedInPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

type RateLimitWindow struct {
	UsedPercent   float64 `json:"usedPercent"`
	WindowMinutes int64   `json:"windowDurationMins"`
	ResetsAt      int64   `json:"resetsAt"`
}

type RateLimits struct {
	Primary   *RateLimitWindow `json:"primary"`
	Secondary *RateLimitWindow `json:"secondary"`
}

func (r *RateLimits) Windows() []RateLimitWindow {
	var windows []RateLimitWindow
	for _, window := range []*RateLimitWindow{r.Primary, r.Secondary} {
		if window != nil {
			windows = append(windows, *window)
		}
	}
	return windows
}

func (w RateLimitWindow) ResetTime() time.Time {
	if w.ResetsAt <= 0 {
		return time.Time{}
	}
	return time.Unix(w.ResetsAt, 0)
}

type appServerMessage struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Uses the same Codex command and credentials as the turns, so it works
// whenever they do.
func (r *Runner) ReadRateLimits(ctx context.Context) (*RateLimits, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, r.Command, "app-server")
	cmd.Env = FilterEnv(os.Environ(), r.PassEnv)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex app-server: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex app-server: stdout pipe: %w", err)
	}
	stderr := &limitedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex app-server: start: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
	}()

	requests := []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"ebi-x","title":null,"version":"1"}}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"account/rateLimits/read"}`,
	}
	if _, err := io.WriteString(stdin, strings.Join(requests, "\n")+"\n"); err != nil {
		return nil, fmt.Errorf("codex app-server: write request: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxAppServerLineBytes)
	for scanner.Scan() {
		var message appServerMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if message.ID == nil || *message.ID != 2 {
			continue
		}
		if message.Error != nil {
			return nil, fmt.Errorf("codex app-server: read rate limits: %s", message.Error.Message)
		}
		var result struct {
			RateLimits *RateLimits `json:"rateLimits"`
		}
		if err := json.Unmarshal(message.Result, &result); err != nil {
			return nil, fmt.Errorf("codex app-server: parse rate limits: %w", err)
		}
		if result.RateLimits == nil {
			return nil, errors.New("codex app-server: no rate limits in response")
		}
		return result.RateLimits, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("codex app-server: %w", ctx.Err())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("codex app-server: read response: %w", err)
	}
	if text := strings.TrimSpace(stderr.String()); text != "" {
		return nil, fmt.Errorf("codex app-server: exited without a response: %s", text)
	}
	return nil, errors.New("codex app-server: exited without a response")
}
