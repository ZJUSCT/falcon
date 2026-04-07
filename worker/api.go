package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// Package-level state set by worker.go before starting the API.
var (
	OnNewAction func(act *shared.Action)
	actionCache *ActionCache
)

func writeJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// HandleDispatchWS handles a dispatch request received via WebSocket.
// Returns (ok, message).
func HandleDispatchWS(da shared.DispatchAction) (bool, string) {
	// Idempotency: check tracker first, then cache.
	if tracker != nil && tracker.Has(da.ID) {
		return true, "already tracked"
	}
	if actionCache != nil {
		if _, found := actionCache.Get(da.ID); found {
			return true, "already known"
		}
	}

	act := &shared.Action{
		ID:               da.ID,
		JobID:            da.JobID,
		Status:           shared.ActionStatusRunning,
		ContainerName:    "syncing-" + da.JobID,
		ContainerImage:   da.ContainerImage,
		ContainerCommand: da.ContainerCommand,
		ContainerVolumes: shared.VolumeList(da.ContainerVolumes),
		ContainerEnv:     da.ContainerEnv,
		ContainerTimeout: da.ContainerTimeout,
		ContainerStatus:  shared.ContainerStatusRunning,
		CreatedAt:        time.Now(),
		StartedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}

	// Docker-level duplicate check: container name = "syncing-{jobID}".
	// If the container is already running, return success regardless of
	// whether it was started by this actionID or a previous one.
	// The existing monitor (if any) will report the result.
	cExists, cRunning, _ := ContainerExistsByName(act.ContainerName)
	if cExists && cRunning {
		return true, "container already running"
	}
	if cExists && !cRunning {
		// Check if the old container's action is still tracked.
		oldTracked := false
		if tracker != nil && !DryRun {
			inspect, err := DockerClient.ContainerInspect(context.Background(), act.ContainerName)
			if err == nil {
				if oldID := inspect.Config.Labels["mirrorgo.action-id"]; oldID != "" {
					if ta := tracker.Get(oldID); ta != nil {
						if ta.Phase == PhasePendingCleanup {
							// Already acked — safe to remove and delete container.
							tracker.Remove(oldID)
						} else {
							// PendingAck — master hasn't received the result yet.
							// Don't delete container; let cleanup handle it after ack.
							oldTracked = true
							log.Warn().Str("container", act.ContainerName).Str("old_action", oldID).Msg("old container still pending ack, skipping removal")
						}
					}
				}
			}
		}
		if oldTracked {
			// Can't start new container while old one exists and is pending ack.
			return false, "previous container pending ack, retry later"
		}
		if DryRun {
			log.Info().Msgf("[dryrun] Would run: docker rm -f %s", act.ContainerName)
		} else {
			DockerClient.ContainerRemove(context.Background(), act.ContainerName, container.RemoveOptions{Force: true})
		}
	}

	if err := StartContainer(act); err != nil {
		return false, "failed to start container: " + err.Error()
	}

	act.ContainerStatus = shared.ContainerStatusRunning

	if OnNewAction != nil {
		OnNewAction(act)
	}

	return true, ""
}
