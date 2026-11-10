package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mad-deecent/attest/internal/perception"
	"github.com/mad-deecent/attest/internal/server"
)

func main() {
	path := flag.String("config", "attest.json", "path to a config file (optional: without one, attest starts unconfigured and is set up through its API)")
	listen := flag.String("listen", "", "address to listen on (default :8420)")
	data := flag.String("data", "", "file attest keeps its state in (default without a config file: attest/attest.json under the user's state directory)")
	reset := flag.Bool("reset-setup", false, "return to first-run setup and print a new setup code, for an operator locked out of settings")
	flag.Parse()
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })

	cfg := server.Config{Listen: ":8420", Fusion: perception.DefaultConfig()}
	raw, err := os.ReadFile(*path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &cfg); err != nil {
			log.Fatalf("parse config: %v", err)
		}
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		// No config file: everything is set at runtime and kept in the
		// data file.
		if *data == "" {
			dir, err := stateDir()
			if err != nil {
				log.Fatalf("no config file and no state directory: %v (pass -data)", err)
			}
			*data = filepath.Join(dir, "attest.json")
		}
		if err := os.MkdirAll(filepath.Dir(*data), 0o700); err != nil {
			log.Fatalf("data directory: %v", err)
		}
		log.Printf("no config file; keeping state in %s", *data)
	default:
		log.Fatalf("read config: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *data != "" {
		cfg.Data = *data
	}
	cfg.ResetSetup = *reset

	srv, err := server.New(cfg, time.Now)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("attest listening on %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, srv.Handler()))
}

// stateDir is $XDG_STATE_HOME/attest, else ~/.local/state/attest.
func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "attest"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "attest"), nil
}
