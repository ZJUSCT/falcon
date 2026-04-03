package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"

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
)

// ContainerInfo holds information about a Docker container discovered during scanning.
type ContainerInfo struct {
	ContainerID   string
	ContainerName string
	ActionID      string // extracted if possible, or empty
	IsRunning     bool
	ExitCode      int
}

// StartContainer creates and starts a Docker container for the given action.
func StartContainer(act *shared.Action) error {
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
	logDir := GetLogDir(act)

	// Remove symlink
	err := os.Remove(filepath.Join(logDir, "container.log"))
	if err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to remove container log symlink")
	}

	// Inspect to get container log path
	inspect, err := DockerClient.ContainerInspect(context.Background(), act.ContainerID)
	if err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to inspect container")
	} else {
		// Copy Docker log to logDir
		err = copyFile(inspect.LogPath, filepath.Join(logDir, "container.log"))
		if err != nil {
			log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to copy container log")
		}
	}

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
