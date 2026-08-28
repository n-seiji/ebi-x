// Command ebiii is the entry point for the ebiii Slack bot.
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

	"github.com/n-seiji/ebiii/internal/codex"
	"github.com/n-seiji/ebiii/internal/config"
	"github.com/n-seiji/ebiii/internal/playbook"
	"github.com/n-seiji/ebiii/internal/policy"
	"github.com/n-seiji/ebiii/internal/slackbot"
	"github.com/n-seiji/ebiii/internal/state"
)

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

	runner := &codex.Runner{
		Command:               cfg.CodexCommand,
		Model:                 cfg.CodexModel,
		ConfigPath:            filepath.Join(cfg.EBIIIHome, ".codex", "config.toml"),
		DeniedReadPaths:       []string{cfg.MemoryDir},
		DeveloperInstructions: policy.Instructions(),
	}
	bot := slackbot.New(nil, store, runner, slackbot.Config{
		AllowedUserIDs:             cfg.AllowedUserIDs,
		AllowedChannelIDs:          cfg.AllowedChannelIDs,
		AllowWorkflows:             cfg.AllowWorkflows,
		AdminUserID:                cfg.AdminUserID,
		WorkspaceDir:               cfg.WorkspaceDir,
		MemoryDir:                  cfg.MemoryDir,
		CodexTimeout:               cfg.CodexTimeout,
		ThreadSubscriptionReaction: cfg.ThreadSubscriptionReaction,
		ThreadSubscriptionTTL:      cfg.ThreadSubscriptionTTL,
		WritableRoots:              cfg.WritableRoots,
	}, playbooks)

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	acceptCtx, stopAccepting := context.WithCancel(context.Background())
	turnCtx, cancelTurns := context.WithCancel(context.Background())
	defer cancelTurns()
	var turns sync.WaitGroup

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
