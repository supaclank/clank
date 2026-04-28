package daemoncli

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/store"
	clanksync "github.com/acksell/clank/internal/sync"
)

// maybeStartSyncAgent reads preferences and, if a remote hub is
// configured AND at least one synced repo is opted in, starts the
// laptop-side sync agent. Returns a stop function (nil when the agent
// is not started). Stop is idempotent.
//
// Reads preferences once at startup. Adding/removing synced repos
// requires a daemon restart for now — config hot-reload is out of MVP.
func maybeStartSyncAgent(ctx context.Context, st *store.Store) (func(), error) {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return nil, fmt.Errorf("load preferences: %w", err)
	}
	if prefs.RemoteHub == nil || strings.TrimSpace(prefs.RemoteHub.URL) == "" {
		return nil, nil // no remote hub configured — agent disabled
	}
	if strings.TrimSpace(prefs.RemoteHub.AuthToken) == "" {
		return nil, fmt.Errorf("remote_hub.url is set but auth_token is empty")
	}
	if len(prefs.SyncedRepos) == 0 {
		log.Printf("sync agent: remote hub configured but no synced_repos — agent idle")
	}
	if st == nil {
		return nil, fmt.Errorf("sync agent requires a SQLite store")
	}

	pusher := clanksync.NewPusher(prefs.RemoteHub.URL, prefs.RemoteHub.AuthToken, nil)
	agent, err := clanksync.NewAgent(clanksync.AgentOptions{
		Repos:  prefs.SyncedRepos,
		Pusher: pusher,
		Store:  st,
		Log:    log.Default(),
	})
	if err != nil {
		return nil, err
	}
	agent.Start(ctx)
	log.Printf("sync agent: pushing to %s for %d repo(s)", prefs.RemoteHub.URL, len(prefs.SyncedRepos))
	return agent.Stop, nil
}
