// Command ebi-x is the entry point for the ebi-x Slack bot.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/n-seiji/ebi-x/internal/codex"
	"github.com/n-seiji/ebi-x/internal/config"
	"github.com/n-seiji/ebi-x/internal/playbook"
	"github.com/n-seiji/ebi-x/internal/policy"
	"github.com/n-seiji/ebi-x/internal/slackbot"
	"github.com/n-seiji/ebi-x/internal/state"
	"github.com/n-seiji/ebi-x/internal/workspace"
)

// checkoutGCInterval is how often idle thread checkouts are looked for.
const checkoutGCInterval = time.Hour

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}
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
		WorkModel:             cfg.CodexWorkModel,
		ConfigPath:            filepath.Join(cfg.EBIXHome, ".codex", "config.toml"),
		DeniedReadPaths:       cfg.ProtectedPaths,
		DeveloperInstructions: policy.Instructions(),
	}
	bot := slackbot.New(nil, store, runner, slackbot.Config{
		AllowedUserIDs:             cfg.AllowedUserIDs,
		AllowedChannelIDs:          cfg.AllowedChannelIDs,
		AllowAllPublicChannels:     cfg.AllowAllPublicChannels,
		AllowWorkflows:             cfg.AllowWorkflows,
		AllowedWorkflowIDs:         cfg.AllowedWorkflowIDs,
		AdminUserID:                cfg.AdminUserID,
		WorkspaceDir:               cfg.WorkspaceDir,
		MemoryDir:                  cfg.MemoryDir,
		PlaybooksDir:               cfg.PlaybooksDir,
		CodexTimeout:               cfg.CodexTimeout,
		ThreadSubscriptionReaction: cfg.ThreadSubscriptionReaction,
		ThreadSubscriptionTTL:      cfg.ThreadSubscriptionTTL,
		WritableRoots:              cfg.WritableRoots,
		MaxParallelWork:            cfg.MaxParallelWork,
		MaxParallelPlan:            cfg.MaxParallelPlan,
		SharedWriteChannelIDs:      cfg.SharedWriteChannelIDs,
		Workspaces:                 workspaces,
	}, playbooks)

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
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

	select {
	case <-signalCtx.Done():
		log.Printf("shutdown requested")
		stopAccepting()
		if err := <-socketDone; err != nil {
			log.Printf("stop Slack Socket Mode: %v", err)
		}
	case err := <-socketDone:
		if err != nil {
			log.Printf("Slack Socket Mode stopped: %v", err)
		}
		stopAccepting()
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
