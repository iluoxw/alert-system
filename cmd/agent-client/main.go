// Command agent-client pulls analysis tasks and runs configured local handlers.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"alert-system/internal/agentclient"
)

func main() {
	config := flag.String("config", "", "path to client YAML")
	flag.Parse()
	if *config == "" {
		flag.Usage()
		os.Exit(2)
	}
	cfg, err := agentclient.LoadConfig(*config)
	if err == nil {
		var c *agentclient.Client
		c, err = agentclient.New(cfg, os.Getenv(cfg.TokenEnv), slog.Default())
		if err == nil {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			err = c.Run(ctx)
			stop()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
