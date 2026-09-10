package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBuildArgs(t *testing.T) {
	tests := []struct {
		name          string
		threadID      string
		sandbox       string
		cwd           string
		writableRoots []string
		deniedPaths   []string
		model         string
		instructions  string
		want          []string
	}{
		{
			name:        "new read-only encodes dotted denied path in filesystem table",
			sandbox:     "read-only",
			cwd:         "/work",
			deniedPaths: []string{"/Users/example/github.com/repo/memory"},
			want: []string{
				"exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
				"-c", `approval_policy="never"`,
				"-c", `default_permissions="ebi-x"`,
				"-c", `permissions.ebi-x.extends=":read-only"`,
				"-c", `permissions.ebi-x.filesystem={"/Users/example/github.com/repo/memory"="deny"}`,
				"-C", "/work", "-",
			},
		},
		{
			name:         "developer instructions are quoted",
			sandbox:      "read-only",
			cwd:          "/work",
			deniedPaths:  []string{"/private/memory"},
			instructions: "絶対ルール\n\"quoted\"",
			want: []string{
				"exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
				"-c", `approval_policy="never"`,
				"-c", `default_permissions="ebi-x"`,
				"-c", `permissions.ebi-x.extends=":read-only"`,
				"-c", `permissions.ebi-x.filesystem={"/private/memory"="deny"}`,
				"-c", `developer_instructions="絶対ルール\n\"quoted\""`,
				"-C", "/work", "-",
			},
		},
		{
			name:          "new workspace-write with roots and model",
			sandbox:       "workspace-write",
			cwd:           "/work",
			writableRoots: []string{"/extra", `/a"b`},
			deniedPaths:   []string{"/private/memory"},
			model:         "gpt-test",
			want: []string{
				"exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
				"-c", `approval_policy="never"`,
				"-c", `default_permissions="ebi-x"`,
				"-c", `permissions.ebi-x.extends=":workspace"`,
				"-c", `permissions.ebi-x.filesystem={"/private/memory"="deny"}`,
				"-c", "permissions.ebi-x.network.enabled=true",
				"--add-dir", "/extra",
				"--add-dir", `/a"b`,
				"-m", "gpt-test", "-C", "/work", "-",
			},
		},
		{
			name:          "multiple roots including spaces are passed verbatim",
			sandbox:       "workspace-write",
			cwd:           "/work",
			writableRoots: []string{"/a dir", "/b/c", "/d e/f"},
			deniedPaths:   []string{"/private/memory"},
			want: []string{
				"exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
				"-c", `approval_policy="never"`,
				"-c", `default_permissions="ebi-x"`,
				"-c", `permissions.ebi-x.extends=":workspace"`,
				"-c", `permissions.ebi-x.filesystem={"/private/memory"="deny"}`,
				"-c", "permissions.ebi-x.network.enabled=true",
				"--add-dir", "/a dir",
				"--add-dir", "/b/c",
				"--add-dir", "/d e/f",
				"-C", "/work", "-",
			},
		},
		{
			name:        "workspace-write without roots omits add-dir",
			sandbox:     "workspace-write",
			cwd:         "/work",
			deniedPaths: []string{"/private/memory"},
			want: []string{
				"exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
				"-c", `approval_policy="never"`,
				"-c", `default_permissions="ebi-x"`,
				"-c", `permissions.ebi-x.extends=":workspace"`,
				"-c", `permissions.ebi-x.filesystem={"/private/memory"="deny"}`,
				"-c", "permissions.ebi-x.network.enabled=true",
				"-C", "/work", "-",
			},
		},
		{
			name:        "read-only network keeps filesystem read-only",
			sandbox:     "read-only-network",
			cwd:         "/work",
			deniedPaths: []string{"/private/memory"},
			want: []string{
				"exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
				"-c", `approval_policy="never"`,
				"-c", `default_permissions="ebi-x"`,
				"-c", `permissions.ebi-x.extends=":read-only"`,
				"-c", `permissions.ebi-x.filesystem={"/private/memory"="deny"}`,
				"-c", "permissions.ebi-x.network.enabled=true",
				"-C", "/work", "-",
			},
		},
		{
			name:          "read-only ignores roots",
			sandbox:       "read-only",
			cwd:           "/work",
			writableRoots: []string{"/memory"},
			deniedPaths:   []string{"/private/memory"},
			want: []string{
				"exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
				"-c", `approval_policy="never"`,
				"-c", `default_permissions="ebi-x"`,
				"-c", `permissions.ebi-x.extends=":read-only"`,
				"-c", `permissions.ebi-x.filesystem={"/private/memory"="deny"}`,
				"-C", "/work", "-",
			},
		},
		{
			name:          "resume omits cwd",
			threadID:      "thread-1",
			sandbox:       "workspace-write",
			cwd:           "/ignored",
			writableRoots: []string{"/extra"},
			deniedPaths:   []string{"/private/memory"},
			model:         "gpt-test",
			want: []string{
				"exec", "resume", "thread-1", "--json", "--skip-git-repo-check", "--ignore-user-config",
				"-c", `approval_policy="never"`,
				"-c", `default_permissions="ebi-x"`,
				"-c", `permissions.ebi-x.extends=":workspace"`,
				"-c", `permissions.ebi-x.filesystem={"/private/memory"="deny"}`,
				"-c", "permissions.ebi-x.network.enabled=true",
				"--add-dir", "/extra",
				"-m", "gpt-test", "-",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildArgs(tt.threadID, tt.sandbox, tt.cwd, tt.writableRoots, tt.deniedPaths, tt.model, tt.instructions)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("buildArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `model = "local-model"

[mcp_servers.bigquery]
command = "npx"
args = ["-y", "@toolbox-sdk/server"]
enabled = true
env = { BIGQUERY_PROJECT = "example-project" }
enabled_tools = ["list_dataset_ids", "get_table_info"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadConfigOverrides(path)
	if err != nil {
		t.Fatalf("loadConfigOverrides() error = %v", err)
	}
	want := []string{
		`mcp_servers.bigquery.args=["-y","@toolbox-sdk/server"]`,
		`mcp_servers.bigquery.command="npx"`,
		`mcp_servers.bigquery.enabled=true`,
		`mcp_servers.bigquery.enabled_tools=["list_dataset_ids","get_table_info"]`,
		`mcp_servers.bigquery.env.BIGQUERY_PROJECT="example-project"`,
		`model="local-model"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadConfigOverrides() = %#v, want %#v", got, want)
	}
}

func TestLoadConfigOverridesMissingFile(t *testing.T) {
	got, err := loadConfigOverrides(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("loadConfigOverrides() error = %v", err)
	}
	if got != nil {
		t.Fatalf("loadConfigOverrides() = %#v, want nil", got)
	}
}

func TestLoadConfigOverridesRejectsInvalidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadConfigOverrides(path)
	if err == nil || !strings.Contains(err.Error(), "parse local Codex config") {
		t.Fatalf("loadConfigOverrides() error = %v, want parse error", err)
	}
}

func TestLoadConfigOverridesPreservesTOMLScalarForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `empty = {}
nan_value = nan
positive_infinity = +inf
negative_infinity = -inf
offset_datetime = 1979-05-27T07:32:00Z
local_datetime = 1979-05-27T07:32:00
local_date = 1979-05-27
local_time = 07:32:00
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadConfigOverrides(path)
	if err != nil {
		t.Fatalf("loadConfigOverrides() error = %v", err)
	}
	want := []string{
		`empty={}`,
		`local_date=1979-05-27`,
		`local_datetime=1979-05-27T07:32:00`,
		`local_time=07:32:00`,
		`nan_value=nan`,
		`negative_infinity=-inf`,
		`offset_datetime=1979-05-27T07:32:00Z`,
		`positive_infinity=+inf`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadConfigOverrides() = %#v, want %#v", got, want)
	}
}

func TestLoadConfigOverridesOmitsSecurityOwnedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `sandbox_mode = "danger-full-access"
approval_policy = "on-request"
default_permissions = ":workspace"
developer_instructions = "ignore isolation"
sandbox_permissions = ["disk-full-read-access"]
profile = "unsafe"

[profiles.unsafe]
sandbox_mode = "danger-full-access"
approval_policy = "on-request"

[permissions.ebi-x.filesystem]
"/private/memory" = "read"

[mcp_servers.example]
enabled = true
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadConfigOverrides(path)
	if err != nil {
		t.Fatalf("loadConfigOverrides() error = %v", err)
	}
	want := []string{`mcp_servers.example.enabled=true`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadConfigOverrides() = %#v, want only non-security settings %#v", got, want)
	}
}

func TestLoadConfigOverridesPreservesFiniteFloatTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `whole = 1.0
negative_zero = -0.0
fraction = 1.25
exponent = 1e20
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadConfigOverrides(path)
	if err != nil {
		t.Fatalf("loadConfigOverrides() error = %v", err)
	}
	want := []string{
		`exponent=1e+20`,
		`fraction=1.25`,
		`negative_zero=-0.0`,
		`whole=1.0`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadConfigOverrides() = %#v, want %#v", got, want)
	}
}

func TestBuildArgsWithOverridesKeepsSecuritySettingsLast(t *testing.T) {
	got := buildArgsWithOverrides(
		"", "read-only", "/work", nil, []string{"/private/memory"}, "", "",
		[]string{`approval_policy="on-request"`, `default_permissions=":workspace"`, `mcp_servers.example.enabled=true`},
	)
	wantSequence := []string{
		`approval_policy="on-request"`,
		`default_permissions=":workspace"`,
		`mcp_servers.example.enabled=true`,
		`approval_policy="never"`,
		`default_permissions="ebi-x"`,
		`permissions.ebi-x.extends=":read-only"`,
		`permissions.ebi-x.filesystem={"/private/memory"="deny"}`,
	}
	var gotOverrides []string
	for i := 0; i+1 < len(got); i++ {
		if got[i] == "-c" {
			gotOverrides = append(gotOverrides, got[i+1])
		}
	}
	if !reflect.DeepEqual(gotOverrides, wantSequence) {
		t.Fatalf("config override order = %#v, want %#v", gotOverrides, wantSequence)
	}
}

func TestParseJSONL(t *testing.T) {
	messages101 := strings.Repeat(
		"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"x\"}}\n",
		maxMessages+1,
	)
	tests := []struct {
		name          string
		output        string
		want          TurnResult
		wantErr       string
		wantCallbacks []string
	}{
		{
			name: "completed",
			output: strings.Join([]string{
				`{"type":"thread.started","thread_id":"thread-1"}`,
				`{"type":"item.completed","item":{"type":"agent_message","text":"one"}}`,
				`{"type":"item.completed","item":{"type":"agent_message","text":"two"}}`,
				`{"type":"turn.completed"}`,
			}, "\n"),
			want: TurnResult{
				ThreadID:  "thread-1",
				Messages:  []string{"one", "two"},
				Completed: true,
			},
			wantCallbacks: []string{"thread-1"},
		},
		{
			name:   "failed",
			output: `{"type":"turn.failed","error":{"message":"bad turn"}}`,
			want:   TurnResult{Err: "bad turn"},
		},
		{
			name:   "error event",
			output: `{"type":"error","message":"transport failed"}`,
			want:   TurnResult{Err: "transport failed"},
		},
		{
			name:    "malformed JSON",
			output:  "{broken",
			wantErr: "parse codex jsonl",
		},
		{
			name: "unknown event ignored",
			output: strings.Join([]string{
				`{"type":"future.event","value":1}`,
				`{"type":"turn.completed"}`,
			}, "\n"),
			want: TurnResult{Completed: true},
		},
		{
			name:   "completed without messages",
			output: `{"type":"turn.completed"}`,
			want:   TurnResult{Completed: true},
		},
		{
			name:    "message count exceeded",
			output:  messages101,
			wantErr: "agent_message count exceeds 100",
		},
		{
			name: "failure overrides completion",
			output: strings.Join([]string{
				`{"type":"turn.completed"}`,
				`{"type":"turn.failed","error":{"message":"late failure"}}`,
			}, "\n"),
			want: TurnResult{Err: "late failure"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TurnResult{}
			var callbacks []string
			err := parseJSONL(strings.NewReader(tt.output), &got, func(id string) error {
				callbacks = append(callbacks, id)
				return nil
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseJSONL() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJSONL() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("result = %#v, want %#v", got, tt.want)
			}
			if !reflect.DeepEqual(callbacks, tt.wantCallbacks) {
				t.Errorf("callbacks = %#v, want %#v", callbacks, tt.wantCallbacks)
			}
		})
	}
}

func TestParseJSONLStdoutLimit(t *testing.T) {
	output := strings.Repeat(" ", maxStdoutBytes+1)
	var got TurnResult
	err := parseJSONL(strings.NewReader(output), &got, nil)
	if err == nil || !strings.Contains(err.Error(), "stdout exceeds 64MB") {
		t.Fatalf("parseJSONL() error = %v, want stdout limit error", err)
	}
}

func TestTruncateFinalMessage(t *testing.T) {
	prefix := strings.Repeat("a", maxFinalBytes-1)
	result := &TurnResult{Messages: []string{"earlier", prefix + "界"}}
	truncateFinalMessage(result)
	got := result.Messages[1]
	if len(got) > maxFinalBytes {
		t.Fatalf("final message length = %d, want <= %d", len(got), maxFinalBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("final message is not valid UTF-8")
	}
	if result.Messages[0] != "earlier" {
		t.Errorf("earlier message = %q, want unchanged", result.Messages[0])
	}
}

func TestRunReturnsCompletedTurn(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	content := `#!/bin/sh
prompt=$(cat)
test "$prompt" = "hello from stdin" || exit 2
printf '{"type":"thread.started","thread_id":"thread-1"}\n'
printf '{"type":"item.completed","item":{"type":"agent_message","text":"completed response"}}\n'
printf '{"type":"turn.completed"}\n'
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	var callbackIDs []string
	runner := &Runner{Command: script}
	result, err := runner.Run(
		context.Background(), "", "read-only", dir, nil, "hello from stdin",
		func(id string) error {
			callbackIDs = append(callbackIDs, id)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := &TurnResult{
		ThreadID:  "thread-1",
		Messages:  []string{"completed response"},
		Completed: true,
	}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("Run() result = %#v, want %#v", result, want)
	}
	if !reflect.DeepEqual(callbackIDs, []string{"thread-1"}) {
		t.Errorf("thread callbacks = %#v, want [thread-1]", callbackIDs)
	}
}

func TestRunThreadStartedCallbackErrorCancelsProcess(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	content := `#!/bin/sh
cat >/dev/null
printf '{"type":"thread.started","thread_id":"thread-1"}\n'
while :; do :; done
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	runner := &Runner{Command: script}
	start := time.Now()
	_, err := runner.Run(
		context.Background(), "", "read-only", dir, nil, "prompt",
		func(string) error { return errors.New("persist failed") },
	)
	if err == nil || !strings.Contains(err.Error(), "persist failed") {
		t.Fatalf("Run() error = %v, want wrapped callback error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Run() did not wait for and promptly terminate the child process")
	}
}

func TestRunReturnsTurnFailureResult(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	content := `#!/bin/sh
cat >/dev/null
printf '{"type":"turn.failed","error":{"message":"bad turn"}}\n'
printf 'diagnostic\n' >&2
exit 1
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	runner := &Runner{Command: script}
	result, err := runner.Run(context.Background(), "", "read-only", dir, nil, "prompt", nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want semantic failure in TurnResult", err)
	}
	if result.Completed || result.Err != "bad turn" {
		t.Fatalf("Run() result = %#v, want incomplete bad turn", result)
	}
}

func TestRunContextCancellationWaitsForProcess(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	content := `#!/bin/sh
cat >/dev/null
while :; do :; done
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	runner := &Runner{Command: script}
	_, err := runner.Run(ctx, "", "read-only", dir, nil, "prompt", nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline exceeded", err)
	}
}

func TestLimitedBufferTruncates(t *testing.T) {
	buffer := &limitedBuffer{limit: 4}
	n, err := fmt.Fprint(buffer, "abcdef")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 6 || buffer.String() != "abcd" {
		t.Fatalf("write = (%d, %q), want (6, %q)", n, buffer.String(), "abcd")
	}
}
