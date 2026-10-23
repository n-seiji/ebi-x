// Command ebi-x is the entry point for the ebi-x Slack bot.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/config"
	"github.com/n-seiji/ebi-x/internal/github"
	"github.com/n-seiji/ebi-x/internal/playbook"
	"github.com/n-seiji/ebi-x/internal/policy"
	"github.com/n-seiji/ebi-x/internal/slackbot"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/n-seiji/ebi-x/internal/workspace"
)

// checkoutGCInterval is how often idle thread checkouts are looked for.
const checkoutGCInterval = time.Hour

// checkConfigArg makes ebi-x only load its configuration and exit, so a
// reload can check the new binary before handing over to it.
const checkConfigArg = "-check-config"

// drainLogInterval is how often a reload reports what it is waiting for.
const drainLogInterval = time.Minute

func main() {
	if len(os.Args) == 2 && os.Args[1] == checkConfigArg {
		if _, err := config.Load(); err != nil {
			log.Fatalf("load configuration: %v", err)
		}
		return
	}
	// config.Load copies .env into the environment; a reload starts from the
	// environment ebi-x was launched with, so edits to .env take effect too.
	launchEnv := os.Environ()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}
	// A reload execs the binary at this path, so a rebuilt binary takes
	// effect without a new process.
	executable, err := os.Executable()
	if err != nil {
		log.Printf("find own executable; reload is unavailable: %v", err)
	}
	releaseLock, err := state.LockDir(cfg.StateDir)
	if err != nil {
		log.Fatalf("lock state directory: %v", err)
	}
	defer releaseLock()
	store, err := state.NewStore(cfg.StateDir)
	if err != nil {
		log.Fatalf("open state store: %v", err)
	}
	if err := store.RecoverStartup(); err != nil {
		log.Fatalf("recover startup state: %v", err)
	}
	if err := store.GC(time.Now(), 7*24*time.Hour); err != nil {
		log.Printf("garbage collect state: %v", err)
	}
	playbooks, err := playbook.List(cfg.PlaybooksDir)
	if err != nil {
		log.Printf("list playbooks: %v", err)
		playbooks = nil
	}

	workspaces, err := workspace.New(context.Background(), cfg.WorkspaceDir, cfg.CheckoutsDir, cfg.WritableRoots)
	if err != nil {
		log.Fatalf("prepare workspaces: %v", err)
	}
	for _, repo := range workspaces.Repositories() {
		log.Printf("work turns use per-thread checkouts of %s", repo)
	}

	runner := &codex.Runner{
		Command:               cfg.CodexCommand,
		Model:                 cfg.CodexModel,
		ConfigPath:            filepath.Join(cfg.EBIXHome, ".codex", "config.toml"),
		DeniedReadPaths:       cfg.ProtectedPaths,
		CodexHome:             cfg.CodexHome,
		DeveloperInstructions: policy.Instructions(),
	}
	var githubAPI slackbot.GitHub
	githubLogin := ""
	var githubRepos []string
	githubToken := cfg.GitHubToken
	if githubToken == "" {
		githubToken = pushToken(cfg.GitHubAPIURL, workspaces.OriginURLs())
	}
	if githubToken != "" {
		client, err := github.NewClient(cfg.GitHubAPIURL, githubToken)
		if err != nil {
			log.Fatalf("configure GitHub: %v", err)
		}
		loginCtx, cancelLogin := context.WithTimeout(context.Background(), 30*time.Second)
		githubLogin, err = client.Login(loginCtx)
		cancelLogin()
		if err != nil {
			// The watch still works; only the token user's own comments
			// may then wake it.
			log.Printf("read GitHub token user: %v", err)
		}
		githubAPI = client
		for _, remote := range workspaces.OriginURLs() {
			if owner, repo, ok := client.RepoFromRemote(remote); ok {
				githubRepos = append(githubRepos, owner+"/"+repo)
			}
		}
		if len(githubRepos) == 0 {
			log.Printf("no writable root has an origin on GitHub; no pull request can be watched")
		}
		log.Printf("watching pull requests of %v on GitHub as %q", githubRepos, githubLogin)
	}
	bot := slackbot.New(nil, store, runner, slackbot.Config{
		AllowedUserIDs:             cfg.AllowedUserIDs,
		AllowedChannelIDs:          cfg.AllowedChannelIDs,
		ApprovalChannelID:          cfg.ApprovalChannelID,
		ApproverUserIDs:            cfg.ApproverUserIDs,
		Approvals:                  store,
		AllowWorkflows:             cfg.AllowWorkflows,
		AllowedWorkflowIDs:         cfg.AllowedWorkflowIDs,
		AdminUserID:                cfg.AdminUserID,
		EBIXHome:                   cfg.EBIXHome,
		WorkspaceDir:               cfg.WorkspaceDir,
		MemoryDir:                  cfg.MemoryDir,
		PlaybooksDir:               cfg.PlaybooksDir,
		ActionRulesFile:            cfg.ActionRulesFile,
		CodexTimeout:               cfg.CodexTimeout,
		ThreadSubscriptionReaction: cfg.ThreadSubscriptionReaction,
		ThreadSubscriptionTTL:      cfg.ThreadSubscriptionTTL,
		WritableRoots:              cfg.WritableRoots,
		MaxParallelWork:            cfg.MaxParallelWork,
		SharedWriteChannelIDs:      cfg.SharedWriteChannelIDs,
		Workspaces:                 workspaces,
		GitHub:                     githubAPI,
		GitHubLogin:                githubLogin,
		GitHubRepos:                githubRepos,
		Usage:                      runner,
	}, playbooks)

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	reloadRequested := make(chan os.Signal, 1)
	if reloadSignal != nil && executable != "" {
		signal.Notify(reloadRequested, reloadSignal)
	}
	acceptCtx, stopAccepting := context.WithCancel(context.Background())
	turnCtx, cancelTurns := context.WithCancel(context.Background())
	defer cancelTurns()
	var turns sync.WaitGroup

	gcDone := make(chan struct{})
	go func() {
		defer close(gcDone)
		collectIdleCheckouts(acceptCtx, workspaces, cfg.CheckoutIdleTTL)
	}()

	socketDone := make(chan error, 1)
	go func() {
		socketDone <- slackbot.RunSocketMode(acceptCtx, turnCtx, cfg.SlackBotToken, cfg.SlackAppToken, bot, &turns)
	}()

	reloading := false
	socketStopped := false
run:
	for {
		select {
		case <-signalCtx.Done():
			log.Printf("shutdown requested")
			break run
		case <-reloadRequested:
			// A binary that cannot start would leave nobody serving Slack
			// once this one exits, so check it before deferring anything.
			if err := checkExecutable(executable, launchEnv); err != nil {
				log.Printf("reload refused; %s does not start: %v", executable, err)
				continue
			}
			// Keep receiving Slack events while running turns finish:
			// requests that arrive meanwhile are saved and started by the
			// new binary.
			log.Printf("reload requested; finishing %d running request(s) before restarting", bot.ActiveRequests())
			reloading = waitForDrain(signalCtx, bot, reloadRequested)
			break run
		case err := <-socketDone:
			if err != nil {
				log.Printf("Slack Socket Mode stopped: %v", err)
			}
			socketStopped = true
			break run
		}
	}
	stopAccepting()
	if !socketStopped {
		if err := <-socketDone; err != nil {
			log.Printf("stop Slack Socket Mode: %v", err)
		}
	}

	<-gcDone

	drained := make(chan struct{})
	go func() {
		turns.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		log.Printf("shutdown complete")
	case <-time.After(60 * time.Second):
		log.Printf("drain timeout; cancelling active turns")
		cancelTurns()
		<-drained
		log.Printf("shutdown complete after forced cancellation")
	}
	if reloading {
		stopSignals()
		signal.Stop(reloadRequested)
		releaseLock()
		log.Printf("restarting from %s", executable)
		if err := reexec(executable, launchEnv); err != nil {
			log.Fatalf("restart from %s: %v", executable, err)
		}
	}
}

// waitForDrain waits until the bot's running requests are done, logging
// what it still waits for. It reports false when a shutdown was requested
// meanwhile, so the process exits instead of restarting.
func waitForDrain(signalCtx context.Context, bot *slackbot.Bot, reloadRequested <-chan os.Signal) bool {
	drained := bot.Drain()
	ticker := time.NewTicker(drainLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-drained:
			log.Printf("running work finished; restarting")
			return true
		case <-signalCtx.Done():
			log.Printf("shutdown requested during reload")
			return false
		case <-reloadRequested:
			log.Printf("reload already in progress; waiting for %d running request(s)", bot.ActiveRequests())
		case <-ticker.C:
			log.Printf("reload waiting for %d running request(s)", bot.ActiveRequests())
		}
	}
}

// checkExecutable runs the binary at path with checkConfigArg and returns
// why it failed, if it did.
func checkExecutable(path string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, checkConfigArg)
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(output))
	}
	return nil
}

// collectIdleCheckouts removes thread checkouts that have not been used for
// idle, at startup and then every checkoutGCInterval until ctx is done.
func collectIdleCheckouts(ctx context.Context, workspaces *workspace.Manager, idle time.Duration) {
	ticker := time.NewTicker(checkoutGCInterval)
	defer ticker.Stop()
	for {
		removed, err := workspaces.GC(time.Now(), idle)
		if err != nil && ctx.Err() == nil {
			log.Printf("remove idle checkouts: %v", err)
		}
		for _, threadID := range removed {
			log.Printf("removed idle checkouts of thread %s", threadID)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pushToken returns the token the work turns push to a GitHub origin with,
// so watching needs no token of its own. It is empty when every origin uses
// SSH or git has no stored credential for it.
func pushToken(apiURL string, origins []string) string {
	client, err := github.NewClient(apiURL, "")
	if err != nil {
		return ""
	}
	for _, origin := range origins {
		if _, _, ok := client.RepoFromRemote(origin); !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		token, err := github.PushToken(ctx, origin)
		cancel()
		if err == nil {
			return token
		}
		log.Printf("read the push credential for %s: %v", redactURL(origin), err)
	}
	return ""
}

// redactURL drops any credential embedded in a remote URL before logging it.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}
