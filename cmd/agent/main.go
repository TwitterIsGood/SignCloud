package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"signcloud/internal/worker"
)

func main() {
	once := flag.Bool("once", false, "claim at most one build, then exit")
	flag.Parse()

	cfg, err := worker.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	agent := worker.New(cfg)
	if err := agent.Run(ctx, *once); err != nil {
		log.Fatal(err)
	}
}
