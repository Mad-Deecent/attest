package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/mad-deecent/attest/internal/gateway"
)

func main() {
	path := flag.String("config", "gateway.json", "path to the config file")
	flag.Parse()

	raw, err := os.ReadFile(*path)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	cfg := gateway.Config{Listen: ":8430"}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	gw, err := gateway.New(cfg, nil)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("attest-gateway listening on %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, gw))
}
