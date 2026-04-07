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
	Vars         map[string]string // e.g. {"BASEDIR": "/home/mirrorgo", "REPODIR": "/mnt/mirrors"}
	DryRun       bool

	dryRunMu    sync.Mutex
	dryRunTimes = make(map[string]dryRunInfo) // action ID -> info
)

// expandVars replaces all $KEY placeholders in s using the Vars map.
func expandVars(s string) string {
	for k, v := range Vars {
		s = strings.ReplaceAll(s, "$"+k, v)
	}
	return s
}

type dryRunInfo struct {
	startTime time.Time
	duration  time.Duration
}

// ContainerInfo holds information about a Docker container discovered during scanning.
type ContainerInfo struct {
	ContainerID      string
	ContainerName    string
	ActionID         string // from label mirrorgo.action-id
	JobID            string // from label mirrorgo.job-id
	IsRunning        bool
	ExitCode         int
	ExitReason       string
	StartedAt        time.Time // actual container start time
	FinishedAt       time.Time // actual container finish time (exited only)
	ContainerTimeout string    // from label mirrorgo.timeout
}

// ContainerExistsByName checks if a Docker container with the given name exists
// and whether it is currently running.
func ContainerExistsByName(name string) (exists bool, running bool, err error) {
	if DryRun {
		log.Info().Str("container", name).Msgf("[dryrun] Would run: docker inspect %s", name)
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

		// Build and log the equivalent docker commands
		var mountArgs []string
		for _, v := range act.ContainerVolumes {
			src := expandVars(v.Source)
			mountArgs = append(mountArgs, fmt.Sprintf("--mount type=bind,source=%s,target=%s", src, v.Destination))
		}
		logDir, err := CreatLogDir(act)
		if err != nil {
			return err
		}
		mountArgs = append(mountArgs, fmt.Sprintf("--mount type=bind,source=%s,target=/mirrorlogs", logDir))

		var envArgs []string
		for _, e := range act.ContainerEnv {
			envArgs = append(envArgs, fmt.Sprintf("-e %s", e))
		}
		envArgs = append(envArgs, "-e MIRRORGO_LOGS_PATH=/mirrorlogs")

		cmdStr := strings.Join(act.ContainerCommand, " ")
		dockerCmd := fmt.Sprintf("docker create --name %s --hostname %s --user root --restart no --network host --log-driver json-file --log-opt max-size=1000m --log-opt max-file=1 --label mirrorgo.action-id=%s --label mirrorgo.job-id=%s --label mirrorgo.timeout=%s %s %s %s %s",
			act.ContainerName, act.ContainerName,
			act.ID, act.JobID, act.ContainerTimeout,
			strings.Join(mountArgs, " "), strings.Join(envArgs, " "),
			act.ContainerImage, cmdStr)
		log.Info().Msgf("[dryrun] Would run: %s", dockerCmd)
		log.Info().Msgf("[dryrun] Would run: docker start %s", act.ContainerName)
		log.Info().Msgf("[dryrun] Would run: docker inspect %s", act.ContainerName)

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

	// Build mounts and env locally — do NOT mutate act.ContainerVolumes/Env
	// so the action struct keeps the original dispatch payload.
	allVolumes := make([]shared.Volume, len(act.ContainerVolumes), len(act.ContainerVolumes)+1)
	copy(allVolumes, act.ContainerVolumes)
	allVolumes = append(allVolumes, shared.Volume{
		Source:      logDir,
		Destination: "/mirrorlogs",
	})

	mounts := make([]mount.Mount, 0, len(allVolumes))
	for _, volume := range allVolumes {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: expandVars(volume.Source),
			Target: volume.Destination,
		})
	}

	env := make([]string, len(act.ContainerEnv), len(act.ContainerEnv)+1)
	copy(env, act.ContainerEnv)
	env = append(env, "MIRRORGO_LOGS_PATH=/mirrorlogs")

	resp, err := DockerClient.ContainerCreate(context.Background(), &container.Config{
		Hostname: act.ContainerName,
		Image:    act.ContainerImage,
		Cmd:      act.ContainerCommand,
		Env:      env,
		Tty:      false,
		User:     "root",
		Labels: map[string]string{
			"mirrorgo.action-id": act.ID,
			"mirrorgo.job-id":    act.JobID,
			"mirrorgo.timeout":   act.ContainerTimeout,
		},
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

	if err != nil {
		act.ContainerStatus = shared.ContainerStatusExited
		act.ContainerExitCode = 11451
		act.ContainerExitReason = "Failed to create container: " + err.Error()
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to create container")
		return err
	}

	act.ContainerID = resp.ID

	if err := DockerClient.ContainerStart(context.Background(), resp.ID, container.StartOptions{}); err != nil {
		act.ContainerStatus = shared.ContainerStatusExited
		act.ContainerExitCode = 11452
		act.ContainerExitReason = "Failed to start container: " + err.Error()
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to start container")
		// Clean up the created-but-not-started container.
		if rmErr := DockerClient.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			log.Error().Err(rmErr).Str("action", act.ID).Msg("Failed to remove container after start failure")
		}
		return err
	}

	inspect, err := DockerClient.ContainerInspect(context.Background(), resp.ID)
	if err != nil {
		// Container is running but we couldn't inspect it for the log path.
		// Not fatal — just skip the log symlink.
		log.Warn().Err(err).Str("job", act.JobID).Str("action", act.ID).Msg("Failed to inspect container after start, skipping log symlink")
		return nil
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
		log.Debug().Msgf("[dryrun] Would run: docker inspect %s", act.ContainerID)
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
		log.Info().Str("action", act.ID).Msgf("[dryrun] Would run: docker inspect %s", act.ContainerID)
		log.Info().Str("action", act.ID).Msgf("[dryrun] Would run: docker rm -f %s", act.ContainerID)
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
		log.Info().Msg("[dryrun] Would run: docker ps -a")
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
			ContainerID:      c.ID,
			ContainerName:    name,
			ActionID:         c.Labels["mirrorgo.action-id"],
			JobID:            c.Labels["mirrorgo.job-id"],
			ContainerTimeout: c.Labels["mirrorgo.timeout"],
		}

		if c.State == "running" {
			info.IsRunning = true
			inspect, inspectErr := DockerClient.ContainerInspect(context.Background(), c.ID)
			if inspectErr == nil && inspect.State != nil {
				if t, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt); err == nil {
					info.StartedAt = t
				}
			}
			running = append(running, info)
		} else if c.State == "exited" {
			// Only recover truly exited containers. Containers in "created"
			// state (never started) are removed — they represent a failed
			// dispatch that crashed before ContainerStart.
			inspect, inspectErr := DockerClient.ContainerInspect(context.Background(), c.ID)
			if inspectErr == nil && inspect.State != nil {
				info.ExitCode = inspect.State.ExitCode
				info.ExitReason = inspect.State.Error
				if t, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt); err == nil {
					info.StartedAt = t
				}
				if t, err := time.Parse(time.RFC3339Nano, inspect.State.FinishedAt); err == nil {
					info.FinishedAt = t
				}
			}
			exited = append(exited, info)
		} else {
			// Container in "created", "paused", "dead", etc. — remove it.
			log.Warn().Str("container", name).Str("state", c.State).Msg("removing container in unexpected state")
			_ = DockerClient.ContainerRemove(context.Background(), c.ID, container.RemoveOptions{Force: true})
		}
	}

	return running, exited, nil
}
