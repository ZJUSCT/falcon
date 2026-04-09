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
	Labels    map[string]string
	Vars      map[string]string // e.g. {"BASEDIR": "/home/mirrorgo", "REPODIR": "/mnt/mirrors"}
	LogDir    string            // defaults to Vars["BASEDIR"] + "/logs/"
	DryRun    bool
}

// package-level tracker reference for use by api.go and ws_client.go
var tracker *Tracker

// Run is the main entry point for the worker process.
func Run(cfg WorkerConfig) {
	// 1. Set up zerolog
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Str("worker", cfg.Name).Logger()

	// Apply defaults
	if cfg.Vars == nil {
		cfg.Vars = make(map[string]string)
	}
	if cfg.LogDir == "" {
		if bd, ok := cfg.Vars["BASEDIR"]; ok {
			cfg.LogDir = filepath.Join(bd, "logs")
		} else {
			cfg.LogDir = "logs"
		}
	}

	// 2. Init Docker client (or skip in dryrun mode)
	if cfg.DryRun {
		DryRun = true
		log.Info().Msg("Dry-run mode active: Docker calls will be simulated")
	} else {
		dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to create Docker client")
		}
		DockerClient = dockerClient
	}

	// 3. Set package-level vars
	Vars = cfg.Vars
	LogDir = cfg.LogDir

	// 4. Create tracker and action cache
	tracker = NewTracker()
	cache := NewActionCache(1000)

	// 5. Create WSClient
	wsClient := NewWSClient(cfg.MasterURL, cfg.Name, cfg.AuthToken, tracker)

	// 6. Wire up callbacks
	onNewAction := func(act *shared.Action) {
		go monitorAction(tracker, act, wsClient, cache)
	}

	wsClient.OnAck = func(actionID string) {
		if tracker.Ack(actionID) {
			log.Debug().Str("action", actionID).Msg("action acked by master")
		}
	}

	wsClient.OnDispatch = func(da shared.DispatchAction) (bool, string) {
		return HandleDispatchWS(da)
	}

	// Set package-level state for api.go
	OnNewAction = onNewAction
	actionCache = cache

	// 7. Register with Master (no Addr needed — all communication via WS)
	regReq := &shared.RegisterRequest{
		Name:   cfg.Name,
		Labels: cfg.Labels,
		Vars:   cfg.Vars,
	}
	if err := register(cfg.MasterURL, cfg.AuthToken, regReq); err != nil {
		log.Fatal().Err(err).Msg("Failed to register with master")
	}
	log.Info().Str("master", cfg.MasterURL).Msg("Registered with master")

	// 8. Scan existing containers and recover state
	running, exited, err := ScanExistingContainers()
	if err != nil {
		log.Error().Err(err).Msg("Failed to scan existing containers")
	} else {
		recoverContainers(running, exited, tracker, wsClient, cache)
	}

	// 9. Start goroutines
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go wsClient.ConnectLoop()
	go heartbeatLoop(ctx, cfg.MasterURL, cfg.AuthToken, cfg.Name, tracker)
	go cleanupLoop(ctx, tracker)
	// ZFS report is now pulled by master on demand; no push loop needed.

	// 10. Wait for SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Info().Str("signal", sig.String()).Msg("Shutting down worker")
	cancel()
}

// recoverContainers processes containers found during startup scan.
func recoverContainers(running, exited []*ContainerInfo, tracker *Tracker, wsClient *WSClient, cache *ActionCache) {
	for _, info := range running {
		if info.ActionID == "" {
			log.Warn().Str("container", info.ContainerName).Msg("Running container has no action-id label, skipping recovery")
			continue
		}
		startedAt := info.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		act := &shared.Action{
			ID:               info.ActionID,
			JobID:            info.JobID,
			ContainerID:      info.ContainerID,
			ContainerName:    info.ContainerName,
			ContainerStatus:  shared.ContainerStatusRunning,
			ContainerTimeout: info.ContainerTimeout,
			Status:           shared.ActionStatusRunning,
			StartedAt:        startedAt,
		}
		tracker.Add(act, PhaseRunning)
		go monitorAction(tracker, act, wsClient, cache)
		log.Info().Str("container", info.ContainerName).Str("action", info.ActionID).Msg("Recovered running container")
	}

	for _, info := range exited {
		if info.ActionID == "" {
			log.Warn().Str("container", info.ContainerName).Msg("Exited container has no action-id label, skipping report")
			continue
		}
		status := shared.ActionStatusSucceeded
		if info.ExitCode != 0 {
			status = shared.ActionStatusFailed
		}
		finishedAt := info.FinishedAt
		if finishedAt.IsZero() {
			finishedAt = time.Now()
		}
		act := &shared.Action{
			ID:                  info.ActionID,
			JobID:               info.JobID,
			ContainerID:         info.ContainerID,
			ContainerName:       info.ContainerName,
			ContainerStatus:     shared.ContainerStatusExited,
			ContainerExitCode:   info.ExitCode,
			ContainerExitReason: info.ExitReason,
			Status:              status,
			StartedAt:           info.StartedAt,
			FinishedAt:          finishedAt,
		}
		tracker.Add(act, PhasePendingAck)
		cache.Put(&CachedActionResult{
			ActionID:   info.ActionID,
			Status:     status,
			ExitCode:   info.ExitCode,
			FinishedAt: finishedAt,
		})
		log.Info().Str("container", info.ContainerName).Str("action", info.ActionID).Int("exit_code", info.ExitCode).Msg("Recovered exited container")
	}
}

// monitorAction monitors a single action's container until it finishes or times out.
func monitorAction(tracker *Tracker, act *shared.Action, wsClient *WSClient, cache *ActionCache) {
	var timeout time.Duration
	if act.ContainerTimeout != "" {
		if d, err := time.ParseDuration(act.ContainerTimeout); err == nil {
			timeout = d
		} else {
			log.Warn().Err(err).Str("action", act.ID).Str("timeout", act.ContainerTimeout).Msg("Invalid timeout, running without limit")
		}
	}

	for {
		if timeout > 0 && time.Since(act.StartedAt) > timeout {
			log.Warn().Str("action", act.ID).Dur("timeout", timeout).Msg("Action timed out, killing container")
			if DockerClient != nil {
				_ = DockerClient.ContainerKill(context.Background(), act.ContainerID, "SIGKILL")
			}
			finishAction(tracker, act, shared.ActionStatusFailed, 137, fmt.Sprintf("timed out after %s", timeout), wsClient, cache)
			return
		}

		finished, err := CheckContainer(act)
		if err != nil {
			log.Error().Err(err).Str("action", act.ID).Msg("Error checking container")
		}
		if finished {
			status := shared.ActionStatusSucceeded
			if act.ContainerExitCode != 0 {
				status = shared.ActionStatusFailed
			}
			finishAction(tracker, act, status, act.ContainerExitCode, act.ContainerExitReason, wsClient, cache)
			return
		}
		time.Sleep(1 * time.Second)
	}
}

// finishAction transitions an action to PendingAck and sends the result.
func finishAction(tracker *Tracker, act *shared.Action, status string, exitCode int, exitReason string, wsClient *WSClient, cache *ActionCache) {
	tracker.Finish(act.ID, status, exitCode, exitReason)

	cache.Put(&CachedActionResult{
		ActionID:   act.ID,
		Status:     status,
		ExitCode:   exitCode,
		ExitReason: exitReason,
		StartedAt:  act.StartedAt,
		FinishedAt: act.FinishedAt,
	})

	wsClient.SendResult(act)
	log.Info().Str("action", act.ID).Str("status", status).Int("exit_code", exitCode).Msg("Action finished")
}

const (
	cleanupScanInterval = 5 * time.Second
	cleanupGracePeriod  = 0 * time.Second
)

var cleanupContainer = CleanupContainer

// heartbeatLoop sends periodic heartbeats to the master.
func heartbeatLoop(ctx context.Context, masterURL, token, name string, tracker *Tracker) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req := shared.HeartbeatRequest{
				Name:           name,
				RunningActions: tracker.RunningIDs(),
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

// cleanupLoop periodically cleans up containers that have been acked.
func cleanupLoop(ctx context.Context, tracker *Tracker) {
	ticker := time.NewTicker(cleanupScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanupDueActions(tracker)
		}
	}
}

func cleanupDueActions(tracker *Tracker) {
	due := tracker.DueCleanup(cleanupGracePeriod)
	for _, act := range due {
		if err := cleanupContainer(act); err != nil {
			log.Warn().Err(err).Str("action", act.ID).Msg("Failed to cleanup container, will retry")
		} else {
			tracker.Remove(act.ID)
			log.Debug().Str("action", act.ID).Msg("Cleaned up container")
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
