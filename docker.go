package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/rs/zerolog/log"
)

var (
	BASEDIR, _ = filepath.Abs(".")
	REPODIR    = "/test1/mirrors/"
)

func StartContainer(act *Action) error {
	log.Debug().Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Starting container")

	logDir, err := CreatLogDir(act)
	if err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to create log dir")
		return err
	}

	act.ContainerVolumes = append(act.ContainerVolumes, Volume{
		Source:      logDir,
		Destination: "/mirrorlogs",
	})

	mounts := []mount.Mount{}

	act.ContainerEnv = append(act.ContainerEnv, "MIRRORGO_LOGS_PATH=/mirrorlogs")

	for _, volume := range act.ContainerVolumes {
		volume.Source = strings.ReplaceAll(volume.Source, "$BASEDIR", BASEDIR)
		volume.Source = strings.ReplaceAll(volume.Source, "$REPODIR", REPODIR)

		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: volume.Source,
			Target: volume.Destination,
		})
	}

	resp, err := dockerClient.ContainerCreate(context.Background(), &container.Config{
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
	}, nil, nil, act.ContainerName)

	act.ContainerID = resp.ID

	if err != nil {
		act.ContainerStatus = ContainerStatusExited
		act.ContainerExitCode = 11451
		act.ContainerExitReason = "Failed to create container: " + err.Error()
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to create container")
		return err
	}

	if err := dockerClient.ContainerStart(context.Background(), resp.ID, container.StartOptions{}); err != nil {
		act.ContainerStatus = ContainerStatusExited
		act.ContainerExitCode = 11452
		act.ContainerExitReason = "Failed to start container: " + err.Error()
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to start container")
		return err
	}

	inspect, err := dockerClient.ContainerInspect(context.Background(), resp.ID)
	if err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to inspect container")
		return err
	}

	os.Link(inspect.LogPath, filepath.Join(logDir, "container.log"))

	return nil
}

func CheckContainer(act *Action) (bool, error) {
	inspect, err := dockerClient.ContainerInspect(context.Background(), act.ContainerID)
	if err != nil {
		act.ContainerStatus = ContainerStatusExited
		act.ContainerExitCode = 11453
		act.ContainerExitReason = "Failed to check container: " + err.Error()
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to inspect container")
		return true, err
	}

	if inspect.State.Status == container.StateExited {
		act.ContainerStatus = ContainerStatusExited
		act.ContainerExitCode = inspect.State.ExitCode
		act.ContainerExitReason = inspect.State.Error

		//copy inspect.LogPath to logs/job_id/action_id.log

		if err := DeleteContainer(act); err != nil {
			log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to delete container")
		}
		return true, nil
	}

	return false, nil
}

func DeleteContainer(act *Action) error {

	if err := dockerClient.ContainerRemove(context.Background(), act.ContainerID, container.RemoveOptions{
		Force: true,
	}); err != nil {
		log.Error().Err(err).Str("job", act.JobID).Str("action", act.ID).Str("image", act.ContainerImage).Msg("Failed to delete container")
		return err
	}
	return nil
}
