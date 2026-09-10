package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(*testing.T, *Config, string)
	}{
		{
			name: "valid",
			env: map[string]string{
				"SLACK_BOT_TOKEN":                    "xoxb-test",
				"SLACK_APP_TOKEN":                    "xapp-test",
				"SLACK_ALLOWED_USER_IDS":             " U123, ,U456 ",
				"SLACK_ALLOWED_CHANNEL_IDS":          " C123, ,C456 ",
				"SLACK_ALLOW_WORKFLOWS":              "true",
				"SLACK_ADMIN_USER_ID":                "UADMIN",
				"CODEX_COMMAND":                      "/usr/local/bin/codex",
				"CODEX_MODEL":                        "gpt-test",
				"CODEX_TIMEOUT":                      "45s",
				"SLACK_THREAD_SUBSCRIPTION_REACTION": "follow-up",
				"SLACK_THREAD_SUBSCRIPTION_TTL":      "48h",
				"EBIII_HOME":                         "data",
			},
			check: func(t *testing.T, cfg *Config, workingDir string) {
				t.Helper()
				wantHome := filepath.Join(workingDir, "data")
				if !reflect.DeepEqual(cfg.AllowedUserIDs, []string{"U123", "U456"}) {
					t.Errorf("AllowedUserIDs = %v, want %v", cfg.AllowedUserIDs, []string{"U123", "U456"})
				}
				if !reflect.DeepEqual(cfg.AllowedChannelIDs, []string{"C123", "C456"}) {
					t.Errorf("AllowedChannelIDs = %v, want %v", cfg.AllowedChannelIDs, []string{"C123", "C456"})
				}
				if !cfg.AllowWorkflows {
					t.Error("AllowWorkflows = false, want true")
				}
				if cfg.AdminUserID != "UADMIN" {
					t.Errorf("AdminUserID = %q, want UADMIN", cfg.AdminUserID)
				}
				if cfg.CodexCommand != "/usr/local/bin/codex" {
					t.Errorf("CodexCommand = %q, want %q", cfg.CodexCommand, "/usr/local/bin/codex")
				}
				if cfg.CodexModel != "gpt-test" {
					t.Errorf("CodexModel = %q, want %q", cfg.CodexModel, "gpt-test")
				}
				if cfg.CodexTimeout != 45*time.Second {
					t.Errorf("CodexTimeout = %v, want %v", cfg.CodexTimeout, 45*time.Second)
				}
				if cfg.ThreadSubscriptionReaction != "follow-up" {
					t.Errorf("ThreadSubscriptionReaction = %q, want %q", cfg.ThreadSubscriptionReaction, "follow-up")
				}
				if cfg.ThreadSubscriptionTTL != 48*time.Hour {
					t.Errorf("ThreadSubscriptionTTL = %v, want %v", cfg.ThreadSubscriptionTTL, 48*time.Hour)
				}
				if cfg.EBIIIHome != wantHome {
					t.Errorf("EBIIIHome = %q, want %q", cfg.EBIIIHome, wantHome)
				}
				assertPaths(t, cfg, wantHome)
			},
		},
		{
			name: "missing bot token",
			env: map[string]string{
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": "U123",
			},
			wantErr: "SLACK_BOT_TOKEN",
		},
		{
			name: "empty allowed users",
			env: map[string]string{
				"SLACK_BOT_TOKEN":        "xoxb-test",
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": " , ",
			},
			wantErr: "SLACK_ALLOWED_USER_IDS",
		},
		{
			name: "invalid allowed user among valid users",
			env: map[string]string{
				"SLACK_BOT_TOKEN":        "xoxb-test",
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": "U123,W456",
			},
			wantErr: `SLACK_ALLOWED_USER_IDS: invalid user ID "W456"`,
		},
		{
			name: "invalid allow workflows",
			env: map[string]string{
				"SLACK_BOT_TOKEN":        "xoxb-test",
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": "U123",
				"SLACK_ALLOW_WORKFLOWS":  "sometimes",
			},
			wantErr: "SLACK_ALLOW_WORKFLOWS",
		},
		{
			name: "invalid admin user",
			env: map[string]string{
				"SLACK_BOT_TOKEN":        "xoxb-test",
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": "U123",
				"SLACK_ADMIN_USER_ID":    "W123",
			},
			wantErr: "SLACK_ADMIN_USER_ID",
		},
		{
			name: "codex command contains whitespace",
			env: map[string]string{
				"SLACK_BOT_TOKEN":        "xoxb-test",
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": "U123",
				"CODEX_COMMAND":          "codex --foo",
			},
			wantErr: "CODEX_COMMAND",
		},
		{
			name: "codex command contains non-space whitespace",
			env: map[string]string{
				"SLACK_BOT_TOKEN":        "xoxb-test",
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": "U123",
				"CODEX_COMMAND":          "codex\t--foo",
			},
			wantErr: "CODEX_COMMAND",
		},
		{
			name: "default home and optional values",
			env: map[string]string{
				"SLACK_BOT_TOKEN":        "xoxb-test",
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": "U123",
			},
			check: func(t *testing.T, cfg *Config, workingDir string) {
				t.Helper()
				if cfg.EBIIIHome != workingDir {
					t.Errorf("EBIIIHome = %q, want %q", cfg.EBIIIHome, workingDir)
				}
				if cfg.CodexCommand != "codex" {
					t.Errorf("CodexCommand = %q, want %q", cfg.CodexCommand, "codex")
				}
				if cfg.CodexTimeout != 30*time.Minute {
					t.Errorf("CodexTimeout = %v, want %v", cfg.CodexTimeout, 30*time.Minute)
				}
				if cfg.ThreadSubscriptionReaction != "thread-subete" {
					t.Errorf("ThreadSubscriptionReaction = %q, want %q", cfg.ThreadSubscriptionReaction, "thread-subete")
				}
				if cfg.ThreadSubscriptionTTL != 336*time.Hour {
					t.Errorf("ThreadSubscriptionTTL = %v, want %v", cfg.ThreadSubscriptionTTL, 336*time.Hour)
				}
				if len(cfg.AllowedChannelIDs) != 0 {
					t.Errorf("AllowedChannelIDs = %v, want %v", cfg.AllowedChannelIDs, []string(nil))
				}
				assertPaths(t, cfg, workingDir)
			},
		},
		{
			name: "invalid timeout",
			env: map[string]string{
				"SLACK_BOT_TOKEN":        "xoxb-test",
				"SLACK_APP_TOKEN":        "xapp-test",
				"SLACK_ALLOWED_USER_IDS": "U123",
				"CODEX_TIMEOUT":          "tomorrow",
			},
			wantErr: "CODEX_TIMEOUT",
		},
		{
			name: "explicit empty subscription reaction disables subscriptions",
			env: map[string]string{
				"SLACK_BOT_TOKEN":                    "xoxb-test",
				"SLACK_APP_TOKEN":                    "xapp-test",
				"SLACK_ALLOWED_USER_IDS":             "U123",
				"SLACK_THREAD_SUBSCRIPTION_REACTION": "",
				"SLACK_THREAD_SUBSCRIPTION_TTL":      "0s",
			},
			check: func(t *testing.T, cfg *Config, workingDir string) {
				t.Helper()
				if cfg.ThreadSubscriptionReaction != "" {
					t.Errorf("ThreadSubscriptionReaction = %q, want disabled empty reaction", cfg.ThreadSubscriptionReaction)
				}
				if cfg.ThreadSubscriptionTTL != 0 {
					t.Errorf("ThreadSubscriptionTTL = %v, want 0 when subscriptions are disabled", cfg.ThreadSubscriptionTTL)
				}
			},
		},
		{
			name: "invalid subscription TTL",
			env: map[string]string{
				"SLACK_BOT_TOKEN":               "xoxb-test",
				"SLACK_APP_TOKEN":               "xapp-test",
				"SLACK_ALLOWED_USER_IDS":        "U123",
				"SLACK_THREAD_SUBSCRIPTION_TTL": "tomorrow",
			},
			wantErr: "SLACK_THREAD_SUBSCRIPTION_TTL",
		},
		{
			name: "non-positive subscription TTL is rejected when subscriptions are enabled",
			env: map[string]string{
				"SLACK_BOT_TOKEN":               "xoxb-test",
				"SLACK_APP_TOKEN":               "xapp-test",
				"SLACK_ALLOWED_USER_IDS":        "U123",
				"SLACK_THREAD_SUBSCRIPTION_TTL": "0s",
			},
			wantErr: "SLACK_THREAD_SUBSCRIPTION_TTL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workingDir := t.TempDir()
			withWorkingDir(t, workingDir)
			workingDir, err := filepath.Abs(".")
			if err != nil {
				t.Fatalf("filepath.Abs() error = %v, want nil", err)
			}
			clearConfigEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			cfg, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			tt.check(t, cfg, workingDir)
		})
	}
}

func TestResolveWritableRoots(t *testing.T) {
	base := t.TempDir()
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v, want nil", base, err)
	}
	target := filepath.Join(realBase, "target")
	other := filepath.Join(realBase, "other")
	for _, dir := range []string{target, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v, want nil", dir, err)
		}
	}
	link := filepath.Join(realBase, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink(%q) error = %v, want nil", link, err)
	}
	file := filepath.Join(realBase, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", file, err)
	}

	home := t.TempDir()
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v, want nil", home, err)
	}
	homeChild := filepath.Join(realHome, "child")
	if err := os.MkdirAll(homeChild, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", homeChild, err)
	}
	t.Setenv("HOME", realHome)

	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr string
	}{
		{name: "empty", in: "  ", want: nil},
		{name: "blank entries and trailing comma", in: target + ", ,," + other + ",", want: []string{target, other}},
		{name: "tilde is expanded", in: "~/child", want: []string{homeChild}},
		{name: "bare tilde is expanded", in: "~", want: []string{realHome}},
		{name: "symlink resolves to its target", in: link, want: []string{target}},
		{name: "duplicates after normalization are dropped", in: link + "," + target + "," + other, want: []string{target, other}},
		{name: "missing path", in: filepath.Join(realBase, "nope"), wantErr: "nope"},
		{name: "file is rejected", in: file, wantErr: "must be a directory"},
		{name: "tilde user is rejected", in: "~someone/dir", wantErr: "~user expansion is not supported"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWritableRoots(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveWritableRoots(%q) error = %v, want containing %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWritableRoots(%q) error = %v, want nil", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("resolveWritableRoots(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLoadWritableRoots(t *testing.T) {
	workingDir := t.TempDir()
	withWorkingDir(t, workingDir)
	clearConfigEnv(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v, want nil", err)
	}
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_ALLOWED_USER_IDS", "U123")
	t.Setenv("EBIII_WRITABLE_ROOTS", root)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(cfg.WritableRoots, []string{root}) {
		t.Errorf("WritableRoots = %v, want %v", cfg.WritableRoots, []string{root})
	}

	t.Setenv("EBIII_WRITABLE_ROOTS", filepath.Join(root, "missing"))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "EBIII_WRITABLE_ROOTS") {
		t.Fatalf("Load() error = %v, want EBIII_WRITABLE_ROOTS failure", err)
	}
}

func TestLoadRejectsMemoryOverlappingWritableRoots(t *testing.T) {
	tests := []struct {
		name     string
		rootPath func(home string) string
	}{
		{name: "memory directory", rootPath: func(home string) string {
			return filepath.Join(home, "data", "memory")
		}},
		{name: "memory parent", rootPath: func(home string) string {
			return filepath.Join(home, "data")
		}},
		{name: "memory child", rootPath: func(home string) string {
			return filepath.Join(home, "data", "memory", "users")
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workingDir := t.TempDir()
			withWorkingDir(t, workingDir)
			clearConfigEnv(t)
			home := filepath.Join(workingDir, "home")
			for _, dir := range []string{
				filepath.Join(home, "data", "workspace"),
				filepath.Join(home, "data", "memory", "users"),
			} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("MkdirAll(%q) error = %v", dir, err)
				}
			}
			t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
			t.Setenv("SLACK_APP_TOKEN", "xapp-test")
			t.Setenv("SLACK_ALLOWED_USER_IDS", "U123")
			t.Setenv("EBIII_HOME", home)
			t.Setenv("EBIII_WRITABLE_ROOTS", tt.rootPath(home))

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "overlaps protected memory directory") {
				t.Fatalf("Load() error = %v, want protected memory overlap", err)
			}
		})
	}
}

func TestLoadRejectsSymlinkedMemoryWritableRoot(t *testing.T) {
	workingDir := t.TempDir()
	withWorkingDir(t, workingDir)
	clearConfigEnv(t)
	home := filepath.Join(workingDir, "home")
	memoryDir := filepath.Join(home, "data", "memory")
	if err := os.MkdirAll(memoryDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", memoryDir, err)
	}
	link := filepath.Join(workingDir, "memory-link")
	if err := os.Symlink(memoryDir, link); err != nil {
		t.Fatalf("Symlink(%q) error = %v", link, err)
	}
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_ALLOWED_USER_IDS", "U123")
	t.Setenv("EBIII_HOME", home)
	t.Setenv("EBIII_WRITABLE_ROOTS", link)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "overlaps protected memory directory") {
		t.Fatalf("Load() error = %v, want protected memory overlap", err)
	}
}

func TestLoadDotEnv(t *testing.T) {
	workingDir := t.TempDir()
	withWorkingDir(t, workingDir)
	clearConfigEnv(t)
	t.Setenv("SLACK_BOT_TOKEN", "from-environment")

	content := "SLACK_BOT_TOKEN=from-file\n" +
		"SLACK_APP_TOKEN='xapp-file'\n" +
		"export SLACK_ALLOWED_USER_IDS=\"U123,U456\"\n"
	if err := os.WriteFile(filepath.Join(workingDir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v, want nil", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.SlackBotToken != "from-environment" {
		t.Errorf("SlackBotToken = %q, want %q", cfg.SlackBotToken, "from-environment")
	}
	if cfg.SlackAppToken != "xapp-file" {
		t.Errorf("SlackAppToken = %q, want %q", cfg.SlackAppToken, "xapp-file")
	}
	if !reflect.DeepEqual(cfg.AllowedUserIDs, []string{"U123", "U456"}) {
		t.Errorf("AllowedUserIDs = %v, want %v", cfg.AllowedUserIDs, []string{"U123", "U456"})
	}
}

func assertPaths(t *testing.T, cfg *Config, home string) {
	t.Helper()
	paths := map[string]string{
		"WorkspaceDir": cfg.WorkspaceDir,
		"MemoryDir":    cfg.MemoryDir,
		"PlaybooksDir": cfg.PlaybooksDir,
		"StateDir":     cfg.StateDir,
	}
	for name, got := range paths {
		want := filepath.Join(home, "data", strings.ToLower(strings.TrimSuffix(name, "Dir")))
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SLACK_BOT_TOKEN",
		"SLACK_APP_TOKEN",
		"SLACK_ALLOWED_USER_IDS",
		"SLACK_ALLOWED_CHANNEL_IDS",
		"SLACK_ALLOW_WORKFLOWS",
		"SLACK_ADMIN_USER_ID",
		"SLACK_THREAD_SUBSCRIPTION_REACTION",
		"SLACK_THREAD_SUBSCRIPTION_TTL",
		"CODEX_COMMAND",
		"CODEX_MODEL",
		"CODEX_TIMEOUT",
		"EBIII_HOME",
		"EBIII_WRITABLE_ROOTS",
	} {
		value, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv(%q) error = %v, want nil", key, err)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(key, value)
				return
			}
			_ = os.Unsetenv(key)
		})
	}
}

func withWorkingDir(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v, want nil", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%q) error = %v, want nil", dir, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(previous)
	})
}

func TestLoadRejectsPlaybooksSymlinkToMemory(t *testing.T) {
	workingDir := t.TempDir()
	withWorkingDir(t, workingDir)
	clearConfigEnv(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_ALLOWED_USER_IDS", "U123")
	memoryDir := filepath.Join(workingDir, "data", "memory")
	if err := os.MkdirAll(memoryDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(memoryDir, filepath.Join(workingDir, "data", "playbooks")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "overlaps protected memory") {
		t.Fatalf("Load() error = %v, want memory isolation error", err)
	}
}
