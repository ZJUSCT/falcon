package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/star/mirrorgo/master"
	"github.com/star/mirrorgo/worker"
)

//go:embed ui/dist/*
var uiFS embed.FS

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: mirrorgo <master|worker> [flags]\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "master":
		cfg := parseMasterFlags(os.Args[2:])
		master.Run(cfg)
	case "worker":
		cfg := parseWorkerFlags(os.Args[2:])
		worker.Run(cfg)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUsage: mirrorgo <master|worker> [flags]\n", os.Args[1])
		os.Exit(1)
	}
}

func parseMasterFlags(args []string) master.MasterConfig {
	cfg := master.MasterConfig{
		Addr:      ":8080",
		DBPath:    "state.db",
		ConfigDir: "Configs",
		BaseDir:   "/home/zjusct/mirrorgo",
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr":
			i++
			cfg.Addr = args[i]
		case "--db":
			i++
			cfg.DBPath = args[i]
		case "--auth-token":
			i++
			cfg.AuthToken = args[i]
		case "--configs":
			i++
			cfg.ConfigDir = args[i]
		case "--basedir":
			i++
			cfg.BaseDir = args[i]
		}
	}
	if cfg.AuthToken == "" {
		cfg.AuthToken = os.Getenv("AUTH_TOKEN")
	}
	// Create sub-filesystem for UI
	uiSub, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create UI sub-filesystem: %v\n", err)
		os.Exit(1)
	}
	cfg.UIFS = uiSub
	return cfg
}

func parseWorkerFlags(args []string) worker.WorkerConfig {
	cfg := worker.WorkerConfig{
		Vars: make(map[string]string),
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++
			cfg.Name = args[i]
		case "--master":
			i++
			cfg.MasterURL = args[i]
		case "--auth-token":
			i++
			cfg.AuthToken = args[i]
		case "--labels":
			i++
			cfg.Labels = parseLabels(args[i])
		case "--basedir":
			// sugar for --var BASEDIR=<value>
			i++
			cfg.Vars["BASEDIR"] = args[i]
		case "--repodir":
			// sugar for --var REPODIR=<value>
			i++
			cfg.Vars["REPODIR"] = args[i]
		case "--var":
			i++
			parts := strings.SplitN(args[i], "=", 2)
			if len(parts) == 2 {
				cfg.Vars[parts[0]] = parts[1]
			}
		case "--dryrun":
			cfg.DryRun = true
		}
	}
	if cfg.AuthToken == "" {
		cfg.AuthToken = os.Getenv("AUTH_TOKEN")
	}
	return cfg
}

func parseLabels(s string) map[string]string {
	labels := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			labels[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return labels
}
