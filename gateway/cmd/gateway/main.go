// Command gateway connects Kaya to WhatsApp.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dhananjay6561/kaya/gateway/internal/config"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	envFile, err := config.LoadEnvFile()
	if err != nil {
		return err
	}
	baseDir, err := os.Getwd()
	if err != nil {
		return err
	}
	if envFile != "" {
		baseDir = filepath.Dir(envFile)
	}

	cfg, err := config.Load(os.Getenv, baseDir)
	if err != nil {
		return err
	}

	fmt.Printf("kaya gateway %s starting (data: %s, port: %d)\n", version, cfg.DataDir, cfg.Port)
	return nil
}
