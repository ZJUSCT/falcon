package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moby/moby/client"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// WorkerConfig holds all configuration for a worker process.
type WorkerConfig struct {
	Name      string
	MasterURL string // e.g. "http://master:8080"
	AuthToken string
	Addr      string // e.g. ":9090"
	Labels    map[string]string
	BaseDir   string
	RepoDir   string
	LogDir    string // defaults to BaseDir + "/logs/"
}

// WorkerState tracks running and acknowledged actions in memory.
type WorkerState struct {
	mu             sync.RWMutex
	runningActions map[string]*shared.Action // action ID -> action
	ackedActions   map[string]time.Time      // action ID -> ack time (for deferred cleanup)
	ackedActionsDB map[string]*shared.Action // action ID -> action (kept for cleanup)
}

func newWorkerState() *WorkerState {
	return &WorkerState{
		runningActions: make(map[string]*shared.Action),
		ackedActions:   make(map[string]time.Time),
		ackedActionsDB: make(map[string]*shared.Action),
	}
}

// Run is the main entry point for the worker process.
func Run(cfg WorkerConfig) {
	// 1. Set up zerolog
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Str("worker", cfg.Name).Logger()

	// Apply defaults
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(cfg.BaseDir, "logs")
	}

	// 2. Init Docker client
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create Docker client")
	}

	// 3. Set package-level vars
	DockerClient = dockerClient
	BaseDir = cfg.BaseDir
	RepoDir = cfg.RepoDir
	LogDir = cfg.LogDir

	// 4. Create ActionCache
	cache := NewActionCache(1000)

	// 5. Create WSClient
	wsClient := NewWSClient(cfg.MasterURL, cfg.Name, cfg.AuthToken)

	// 6. Set up in-memory action tracking
	ws := newWorkerState()

	// 7. Wire up callbacks
	onNewAction := func(act *shared.Action) {
		ws.mu.Lock()
		ws.runningActions[act.ID] = act
		ws.mu.Unlock()
		go monitorAction(ws, act, wsClient, cache)
	}

	wsClient.OnAck = func(actionID string) {
		ws.mu.Lock()
		defer ws.mu.Unlock()
		if act, ok := ws.runningActions[actionID]; ok {
			ws.ackedActions[actionID] = time.Now()
			ws.ackedActionsDB[actionID] = act
			delete(ws.runningActions, actionID)
		}
	}

	SetWorkerAPIState(cache, cfg.AuthToken, onNewAction)

	// 8. Register with Master
	// Construct a reachable address for the worker. If cfg.Addr is just a port
	// (e.g. ":9090"), build a URL using the worker name as hostname (works in
	// Docker Compose where container names are DNS-resolvable).
	regAddr := cfg.Addr
	if strings.HasPrefix(regAddr, ":") {
		regAddr = "http://" + cfg.Name + regAddr
	} else if !strings.HasPrefix(regAddr, "http://") && !strings.HasPrefix(regAddr, "https://") {
		regAddr = "http://" + regAddr
	}
	regReq := &shared.RegisterRequest{
		Name:   cfg.Name,
		Labels: cfg.Labels,
		Addr:   regAddr,
	}
	if err := register(cfg.MasterURL, cfg.AuthToken, regReq); err != nil {
		log.Fatal().Err(err).Msg("Failed to register with master")
	}
	log.Info().Str("master", cfg.MasterURL).Msg("Registered with master")

	// 9. Scan existing containers
	running, exited, err := ScanExistingContainers()
	if err != nil {
		log.Error().Err(err).Msg("Failed to scan existing containers")
	} else {
		for _, info := range running {
			act := &shared.Action{
				ID:              info.ActionID,
				ContainerID:     info.ContainerID,
				ContainerName:   info.ContainerName,
				ContainerStatus: shared.ContainerStatusRunning,
				Status:          shared.ActionStatusRunning,
				StartedAt:       time.Now(),
			}
			if act.ID == "" {
				act.ID = info.ContainerName // fallback
			}
			ws.mu.Lock()
			ws.runningActions[act.ID] = act
			ws.mu.Unlock()
			go monitorAction(ws, act, wsClient, cache)
			log.Info().Str("container", info.ContainerName).Msg("Recovered running container")
		}
		for _, info := range exited {
			result := &CachedActionResult{
				ActionID:   info.ContainerName,
				Status:     shared.ActionStatusSucceeded,
				ExitCode:   info.ExitCode,
				FinishedAt: time.Now(),
			}
			if info.ExitCode != 0 {
				result.Status = shared.ActionStatusFailed
			}
			cache.Put(result)
			wsClient.Send(&shared.WSMessage{
				Type:            "action_result",
				ActionID:        info.ContainerName,
				Status:          result.Status,
				ContainerStatus: shared.ContainerStatusExited,
				ExitCode:        info.ExitCode,
				UpdatedAt:       time.Now(),
			})
			log.Info().Str("container", info.ContainerName).Int("exit_code", info.ExitCode).Msg("Reported exited container")
		}
	}

	// 10. Start goroutines
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go wsClient.ConnectLoop()
	go heartbeatLoop(ctx, cfg.MasterURL, cfg.AuthToken, cfg.Name, ws)
	go cleanupLoop(ctx, ws)
	go func() {
		if err := StartWorkerAPI(cfg.Addr, cfg.AuthToken); err != nil {
			log.Fatal().Err(err).Msg("Worker API server failed")
		}
	}()

	// 11. Wait for SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Info().Str("signal", sig.String()).Msg("Shutting down worker")
	cancel()
}

// monitorAction monitors a single action's container until it finishes.
func monitorAction(ws *WorkerState, act *shared.Action, wsClient *WSClient, cache *ActionCache) {
	for {
		finished, err := CheckContainer(act)
		if err != nil {
			log.Error().Err(err).Str("action", act.ID).Msg("Error checking container")
		}
		if finished {
			status := shared.ActionStatusSucceeded
			if act.ContainerExitCode != 0 {
				status = shared.ActionStatusFailed
			}
			act.Status = status
			act.FinishedAt = time.Now()

			cache.Put(&CachedActionResult{
				ActionID:   act.ID,
				Status:     status,
				ExitCode:   act.ContainerExitCode,
				ExitReason: act.ContainerExitReason,
				StartedAt:  act.StartedAt,
				FinishedAt: act.FinishedAt,
			})

			wsClient.Send(&shared.WSMessage{
				Type:            "action_result",
				ActionID:        act.ID,
				Status:          status,
				ContainerStatus: shared.ContainerStatusExited,
				ExitCode:        act.ContainerExitCode,
				ExitReason:      act.ContainerExitReason,
				UpdatedAt:       time.Now(),
			})

			// Keep the action in runningActions so the heartbeat reports it
			// until the master sends an ack. OnAck handles the actual removal.

			log.Info().Str("action", act.ID).Str("status", status).Int("exit_code", act.ContainerExitCode).Msg("Action finished")
			return
		}
		time.Sleep(1 * time.Second)
	}
}

// heartbeatLoop sends periodic heartbeats to the master.
func heartbeatLoop(ctx context.Context, masterURL, token, name string, ws *WorkerState) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ws.mu.RLock()
			ids := make([]string, 0, len(ws.runningActions))
			for id := range ws.runningActions {
				ids = append(ids, id)
			}
			ws.mu.RUnlock()

			req := shared.HeartbeatRequest{
				Name:           name,
				RunningActions: ids,
			}
			body, _ := json.Marshal(req)

			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
				masterURL+"/api/internal/heartbeat", bytes.NewReader(body))
			if err != nil {
				log.Warn().Err(err).Msg("Failed to create heartbeat request")
				continue
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(httpReq)
			if err != nil {
				log.Warn().Err(err).Msg("Heartbeat failed")
				continue
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				log.Warn().Int("status", resp.StatusCode).Msg("Heartbeat returned non-OK status")
			}
		}
	}
}

// cleanupLoop periodically cleans up acknowledged containers.
func cleanupLoop(ctx context.Context, ws *WorkerState) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ws.mu.Lock()
			var toClean []string
			for id, ackedAt := range ws.ackedActions {
				if time.Since(ackedAt) > 1*time.Hour {
					toClean = append(toClean, id)
				}
			}
			// Collect actions to clean while holding the lock, then release.
			cleanActions := make(map[string]*shared.Action)
			for _, id := range toClean {
				if act, ok := ws.ackedActionsDB[id]; ok {
					cleanActions[id] = act
				}
				delete(ws.ackedActions, id)
				delete(ws.ackedActionsDB, id)
			}
			ws.mu.Unlock()

			for id, act := range cleanActions {
				if err := CleanupContainer(act); err != nil {
					log.Warn().Err(err).Str("action", id).Msg("Failed to cleanup container")
				} else {
					log.Debug().Str("action", id).Msg("Cleaned up acked container")
				}
			}
		}
	}
}

// register sends a registration request to the master with retry and backoff.
func register(masterURL, token string, req *shared.RegisterRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal register request: %w", err)
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	const maxAttempts = 10

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		httpReq, err := http.NewRequest(http.MethodPost, masterURL+"/api/internal/register", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			log.Warn().Err(err).Int("attempt", attempt).Msg("Registration failed, retrying")
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return nil
		}

		log.Warn().Int("status", resp.StatusCode).Int("attempt", attempt).Msg("Registration returned non-OK, retrying")
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	return fmt.Errorf("registration failed after %d attempts", maxAttempts)
}
