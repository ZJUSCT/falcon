package main

import (
	"os"
	"path/filepath"
)

var LogDir = "./logs/"

func CreatLogDir(act *Action) (string, error) {

	// dir := filepath.Join(LogDir, act.JobID, act.ID)
	dir := filepath.Join(LogDir, act.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	// get absolute path
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	return dir, nil
}

func GetLogDir(act *Action) string {
	// return filepath.Join(LogDir, act.JobID, act.ID)
	return filepath.Join(LogDir, act.ID)
}
