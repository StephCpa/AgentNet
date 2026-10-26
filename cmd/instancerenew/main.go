package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/agentnet/agentnet/internal/credentialclient"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func run() error {
	config := flag.String("config", "", "workload renewal configuration JSON")
	watch := flag.Bool("watch", false, "renew before expiry and retry pending work until stopped")
	flag.Parse()
	if *config == "" {
		return fmt.Errorf("-config is required")
	}
	file, err := os.Open(*config)
	if err != nil {
		return err
	}
	defer file.Close()
	var c credentialclient.Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return fmt.Errorf("invalid renewal configuration")
	}
	base := filepath.Dir(*config)
	for _, name := range []*string{&c.ServerCAFile, &c.BundleFile, &c.StateDir} {
		if *name != "" && !filepath.IsAbs(*name) {
			*name = filepath.Join(base, *name)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *watch {
		err := credentialclient.Watch(ctx, c, func(err error) { log.Printf("renewal pending: %v", err) })
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	attempt, stop := context.WithTimeout(ctx, 45*time.Second)
	defer stop()
	err = credentialclient.Renew(attempt, c)
	if errors.Is(err, credentialclient.ErrAbandoned) {
		err = credentialclient.Renew(attempt, c)
	}
	if err != nil {
		return err
	}
	log.Print("Credential renewed; reconnect workload clients using the current bundle")
	return nil
}
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
