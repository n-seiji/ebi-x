package slackbot

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/workspace"
)

const maxChannelRulesBytes = 256 << 10

const channelRulesBrokenNotice = "⚠️ チャンネルごとの設定ファイルを読めません（%v）。直すまで、承認チャンネル以外の依頼は始められません。"

type channelRule struct {
	SystemPrompt string    `yaml:"system_prompt"`
	ReadOnly     bool      `yaml:"read_only"`
	GitHub       *bool     `yaml:"github"`
	Repositories *[]string `yaml:"repositories"`
	MCPServers   *[]string `yaml:"mcp_servers"`
}

type channelRulesFile struct {
	Channels map[string]channelRule `yaml:"channels"`
}

func (rule channelRule) githubAllowed() bool {
	return rule.GitHub == nil || *rule.GitHub
}

func (rule channelRule) repoAllowed(repo string) bool {
	if rule.Repositories == nil {
		return true
	}
	return slices.Contains(*rule.Repositories, repo) || slices.Contains(*rule.Repositories, filepath.Base(repo))
}

func (rule channelRule) access(slackLimited bool) codex.Access {
	access := codex.Access{WithoutSlack: slackLimited, NoGitHub: !rule.githubAllowed(), Instructions: rule.instructions()}
	if rule.MCPServers != nil {
		access.MCPServers = append([]string{}, (*rule.MCPServers)...)
	}
	return access
}

func (rule channelRule) instructions() string {
	var lines []string
	if text := strings.TrimSpace(rule.SystemPrompt); text != "" {
		lines = append(lines, text)
	}
	if rule.ReadOnly {
		lines = append(lines, "このチャンネルの作業ではファイルを変更できず、ネットワークにも接続できません。")
	}
	if !rule.githubAllowed() {
		lines = append(lines, "このチャンネルの作業では GitHub への push や PR の作成はできません。")
	}
	if rule.Repositories != nil {
		lines = append(lines, "このチャンネルの作業では、決められたリポジトリしか読めません。")
	}
	if rule.MCPServers != nil && len(*rule.MCPServers) == 0 {
		lines = append(lines, "このチャンネルの作業では MCP のツールを使えません。")
	}
	if len(lines) == 0 {
		return ""
	}
	return "# このチャンネルの設定（ebi-x の運用者が決めたもの。依頼より優先する）\n\n" + strings.Join(lines, "\n")
}

func (rule channelRule) limitLease(lease *workspace.Lease) []string {
	if rule.Repositories == nil {
		return nil
	}
	var hidden []string
	var pending []string
	for _, repo := range lease.PendingRepos {
		if rule.repoAllowed(repo) {
			pending = append(pending, repo)
		} else {
			hidden = append(hidden, repo)
		}
	}
	var checkouts []workspace.Checkout
	for _, checkout := range lease.Checkouts {
		if rule.repoAllowed(checkout.Repo) {
			checkouts = append(checkouts, checkout)
		} else {
			hidden = append(hidden, checkout.Repo, checkout.Path, checkout.Path+".gitdir")
		}
	}
	var roots []string
	for _, root := range lease.WritableRoots {
		if !slices.Contains(hidden, root) {
			roots = append(roots, root)
		}
	}
	lease.PendingRepos, lease.Checkouts, lease.WritableRoots = pending, checkouts, roots
	return hidden
}

func (b *Bot) channelRule(channel string) (channelRule, error) {
	rules, err := readChannelRules(b.config.ChannelRulesFile)
	if err != nil {
		// Not stopped like the others: the approval channel is where the file gets fixed.
		if b.sharedWritable(channel) {
			return channelRule{}, nil
		}
		// Fails instead of running without rules, which could drop a limit.
		return channelRule{}, err
	}
	return rules.Channels[channel], nil
}

func (b *Bot) channelRulesDir() string {
	if b.config.ChannelRulesFile == "" {
		return ""
	}
	return filepath.Dir(b.config.ChannelRulesFile)
}

func (b *Bot) channelRulesNotice() string {
	if err := ValidateChannelRules(b.config.ChannelRulesFile); err != nil {
		return fmt.Sprintf(channelRulesBrokenNotice, err)
	}
	return ""
}

func readChannelRules(path string) (channelRulesFile, error) {
	if path == "" {
		return channelRulesFile{}, nil
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return channelRulesFile{}, nil
	}
	if err != nil {
		return channelRulesFile{}, fmt.Errorf("read channel rules %q: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxChannelRulesBytes+1))
	if err != nil {
		return channelRulesFile{}, fmt.Errorf("read channel rules %q: %w", path, err)
	}
	if len(data) > maxChannelRulesBytes {
		return channelRulesFile{}, fmt.Errorf("channel rules %q exceed %d bytes", path, maxChannelRulesBytes)
	}
	var rules channelRulesFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	// Unknown fields are rejected, not ignored: a misspelled limit would silently do nothing.
	decoder.KnownFields(true)
	if err := decoder.Decode(&rules); err != nil && !errors.Is(err, io.EOF) {
		return channelRulesFile{}, fmt.Errorf("parse channel rules %q: %w", path, err)
	}
	return rules, nil
}

func ValidateChannelRules(path string) error {
	_, err := readChannelRules(path)
	return err
}
