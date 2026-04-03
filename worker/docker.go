package worker

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

var (
	DockerClient *client.Client
	BaseDir      string
	RepoDir      string
	DryRun       bool

	dryRunMu    sync.Mutex
	dryRunTimes = make(map[string]dryRunInfo) // action ID -> info
)

type dryRunInfo struct {
	startTime time.Time
	duration  time.Duration
}

// ContainerInfo holds information about a Docker container discovered during scanning.
type ContainerInfo struct {
	ContainerID   string
	ContainerName string
	ActionID      string // extracted if possible, or empty
	IsRunning     bool
	ExitCode      int
}

// ContainerExistsByName checks if a Docker container with the given name exists
// and whether it is currently running.
func ContainerExistsByName(name string) (exists bool, running bool, err error) {
	if DryRun {
		return false, false, nil
	}
	inspect, err := DockerClient.ContainerInspect(context.Background(), name)
	if err != nil {
		// If the error indicates the container doesn't exist, return false.
		if errdefs.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, inspect.State.Running, nil
}

// StartContainer creates and starts a Docker container for the given action.
func StartContainer(act *shared.Action) error {
	if DryRun {
		log.Info().Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("[dryrun] Simulating container start")

		logDir, err := CreatLogDir(act)
		if err != nil {
			return err
		}
		// Write a dummy log file so log endpoints work
		_ = os.WriteFile(filepath.Join(logDir, "container.log"), []byte(fmt.Sprintf("dryrun: simulating container for %s\n", act.JobID)), 0644)

		act.ContainerID = "dryrun-" + act.ID
		act.ContainerStatus = shared.ContainerStatusRunning

		duration := time.Duration(5+rand.Intn(11)) * time.Second
		dryRunMu.Lock()
		dryRunTimes[act.ID] = dryRunInfo{startTime: time.Now(), duration: duration}
		dryRunMu.Unlock()

		log.Info().Str("action", act.ID).Dur("simulated_duration", duration).Msg("[dryrun] Container started (simulated)")
		return nil
	}

	log.Debug().Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Starting container")

	logDir, err := CreatLogDir(act)
	if err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to create log dir")
		return err
	}

	act.ContainerVolumes = append(act.ContainerVolumes, shared.Volume{
		Source:      logDir,
		Destination: "/mirrorlogs",
	})

	mounts := []mount.Mount{}

	act.ContainerEnv = append(act.ContainerEnv, "MIRRORGO_LOGS_PATH=/mirrorlogs")

	for _, volume := range act.ContainerVolumes {
		volume.Source = strings.ReplaceAll(volume.Source, "$BASEDIR", BaseDir)
		volume.Source = strings.ReplaceAll(volume.Source, "$REPODIR", RepoDir)

		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: volume.Source,
			Target: volume.Destination,
		})
	}

	resp, err := DockerClient.ContainerCreate(context.Background(), &container.Config{
		Hostname: act.ContainerName,
		Image:    act.ContainerImage,
		Cmd:      act.ContainerCommand,
		Env:      act.ContainerEnv,
		Tty:      false,
		User:     "root",
	}, &container.HostConfig{
		RestartPolicy: container.RestartPolicy{
			Name: "no",
		},
		Mounts: mounts,
		LogConfig: container.LogConfig{
			Type: "json-file",
			Config: map[string]string{
				"max-size": "1000m",
				"max-file": "1",
			},
		},
		NetworkMode: "host",
	}, nil, nil, act.ContainerName)

	act.ContainerID = resp.ID

	if err != nil {
		act.ContainerStatus = shared.ContainerStatusExited
		act.ContainerExitCode = 11451
		act.ContainerExitReason = "Failed to create container: " + err.Error()
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to create container")
		return err
	}

	if err := DockerClient.ContainerStart(context.Background(), resp.ID, container.StartOptions{}); err != nil {
		act.ContainerStatus = shared.ContainerStatusExited
		act.ContainerExitCode = 11452
		act.ContainerExitReason = "Failed to start container: " + err.Error()
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to start container")
		return err
	}

	inspect, err := DockerClient.ContainerInspect(context.Background(), resp.ID)
	if err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to inspect container")
		return err
	}

	err = os.Symlink(inspect.LogPath, filepath.Join(logDir, "container.log"))
	if err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to link container log")
	}

	return nil
}

// CheckContainer inspects the container and records exit status if finished.
// It does NOT delete the container — deletion is deferred to CleanupContainer.
func CheckContainer(act *shared.Action) (bool, error) {
	if DryRun {
		dryRunMu.Lock()
		info, ok := dryRunTimes[act.ID]
		dryRunMu.Unlock()
		if !ok {
			// No tracking info; treat as finished
			act.ContainerStatus = shared.ContainerStatusExited
			act.ContainerExitCode = 0
			return true, nil
		}
		if time.Since(info.startTime) >= info.duration {
			act.ContainerStatus = shared.ContainerStatusExited
			act.ContainerExitCode = 0
			act.ContainerExitReason = ""
			dryRunMu.Lock()
			delete(dryRunTimes, act.ID)
			dryRunMu.Unlock()
			log.Info().Str("action", act.ID).Msg("[dryrun] Container finished (simulated)")
			return true, nil
		}
		return false, nil
	}

	inspect, err := DockerClient.ContainerInspect(context.Background(), act.ContainerID)
	if err != nil {
		act.ContainerStatus = shared.ContainerStatusExited
		act.ContainerExitCode = 11453
		act.ContainerExitReason = "Failed to check container: " + err.Error()
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to inspect container")
		return true, err
	}

	if inspect.State.Status == container.StateExited {
		act.ContainerStatus = shared.ContainerStatusExited
		act.ContainerExitCode = inspect.State.ExitCode
		act.ContainerExitReason = inspect.State.Error
		return true, nil
	}

	return false, nil
}

// DeleteContainer removes the container's log symlink, copies the Docker log
// to the action log directory, and removes the container.
func DeleteContainer(act *shared.Action) error {
	if DryRun {
		log.Info().Str("action", act.ID).Msg("[dryrun] Skipping container deletion (simulated)")
		return nil
	}

	logDir := GetLogDir(act)

	// 1. Inspect to get container log path (before removing anything)
	inspect, err := DockerClient.ContainerInspect(context.Background(), act.ContainerID)
	if err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to inspect container")
		return fmt.Errorf("inspect container for log path: %w", err)
	}

	// 2. Copy Docker log to logDir (before removing the container)
	logDest := filepath.Join(logDir, "container.log.tmp")
	if err := copyFile(inspect.LogPath, logDest); err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to copy container log; skipping container removal to preserve log")
		return fmt.Errorf("copy container log: %w", err)
	}

	// 3. Remove symlink and put the real copy in place
	symlink := filepath.Join(logDir, "container.log")
	_ = os.Remove(symlink) // remove symlink (best-effort)
	if err := os.Rename(logDest, symlink); err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Msg("Failed to rename copied log")
	}

	// 4. Force remove container (safe now — we have the log copy)
	if err := DockerClient.ContainerRemove(context.Background(), act.ContainerID, container.RemoveOptions{
		Force: true,
	}); err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to delete container")
		return err
	}
	return nil
}

// CleanupContainer is the deferred cleanup entry point. It removes the
// container and its associated resources after the Master has acknowledged
// the action result.
func CleanupContainer(act *shared.Action) error {
	return DeleteContainer(act)
}

// ScanExistingContainers lists all Docker containers with the "syncing-" name
// prefix and returns them split into running and exited slices. Matching
// containers to specific actions is left to the caller.
func ScanExistingContainers() (running []*ContainerInfo, exited []*ContainerInfo, err error) {
	if DryRun {
		log.Info().Msg("[dryrun] Skipping container scan (no containers in dryrun mode)")
		return nil, nil, nil
	}

	containers, err := DockerClient.ContainerList(context.Background(), container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, nil, err
	}

	for _, c := range containers {
		// Docker container names are prefixed with "/" in the API response.
		name := ""
		for _, n := range c.Names {
			trimmed := strings.TrimPrefix(n, "/")
			if strings.HasPrefix(trimmed, "syncing-") {
				name = trimmed
				break
			}
		}
		if name == "" {
			continue
		}

		info := &ContainerInfo{
			ContainerID:   c.ID,
			ContainerName: name,
		}

		if c.State == "running" {
			info.IsRunning = true
			running = append(running, info)
		} else {
			// For exited containers, inspect to get the exit code.
			inspect, inspectErr := DockerClient.ContainerInspect(context.Background(), c.ID)
			if inspectErr == nil && inspect.State != nil {
				info.ExitCode = inspect.State.ExitCode
			}
			exited = append(exited, info)
		}
	}

	return running, exited, nil
}
