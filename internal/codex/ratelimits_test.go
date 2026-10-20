package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFakeAppServer(t *testing.T, response string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "codex")
	// Like the real app server, it answers each request and keeps running
	// until stdin closes.
	content := `#!/bin/sh
test "$1" = "app-server" || exit 2
read -r init
printf '{"id":1,"result":{"userAgent":"ebi-x/0"}}\n'
read -r initialized
read -r request
case "$request" in *account/rateLimits/read*) ;; *) exit 3 ;; esac
printf '{"method":"configWarning","params":{"summary":"x"}}\n'
printf '%s\n' '` + response + `'
cat >/dev/null
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return script
}

func TestReadRateLimits(t *testing.T) {
	script := writeFakeAppServer(t, `{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":62.5,"windowDurationMins":300,"resetsAt":1792470000},"secondary":{"usedPercent":12,"windowDurationMins":10080,"resetsAt":1792900000},"credits":null},"rateLimitsByLimitId":null}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	limits, err := (&Runner{Command: script}).ReadRateLimits(ctx)
	if err != nil {
		t.Fatalf("ReadRateLimits() error = %v", err)
	}
	if limits.Primary == nil || limits.Primary.UsedPercent != 62.5 || limits.Primary.WindowMinutes != 300 || limits.Primary.ResetsAt != 1792470000 {
		t.Fatalf("primary = %+v", limits.Primary)
	}
	if limits.Secondary == nil || limits.Secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary = %+v", limits.Secondary)
	}
	if got := len(limits.Windows()); got != 2 {
		t.Fatalf("windows = %d, want 2", got)
	}
}

func TestReadRateLimitsError(t *testing.T) {
	script := writeFakeAppServer(t, `{"error":{"code":-32600,"message":"codex account authentication required to read rate limits"},"id":2}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := (&Runner{Command: script}).ReadRateLimits(ctx)
	if err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("ReadRateLimits() error = %v", err)
	}
}

func TestIsUsageLimitMessage(t *testing.T) {
	if !IsUsageLimitMessage("You’ve hit your usage limit. Try again at 3:30 PM.") {
		t.Fatal("usage limit message not recognized")
	}
	if IsUsageLimitMessage("stream disconnected before completion") {
		t.Fatal("unrelated error recognized as usage limit")
	}
}
