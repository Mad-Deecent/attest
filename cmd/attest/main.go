package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/mad-deecent/attest/internal/perception"
	"github.com/mad-deecent/attest/internal/server"
)

func main() {
	path := flag.String("config", "attest.json", "path to the config file")
	flag.Parse()

	raw, err := os.ReadFile(*path)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	cfg := server.Config{Listen: ":8420", Fusion: perception.DefaultConfig()}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	srv, err := server.New(cfg, time.Now)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("attest listening on %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, srv.Handler()))
}
