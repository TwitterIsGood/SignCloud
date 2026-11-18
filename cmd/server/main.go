package main

import (
	"log"
	"os"

	"signcloud/internal/control"
)

func main() {
	cfg, err := control.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	server, err := control.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()

	log.Printf("SignCloud control plane listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil {
		log.Printf("server stopped: %v", err)
		os.Exit(1)
	}
}
