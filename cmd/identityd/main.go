package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentnet/agentnet/services/identity"
)

func run() error {
	config := flag.String("config", "", "identity service configuration JSON")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	startup, stop := context.WithTimeout(ctx, 20*time.Second)
	server, db, err := identity.OpenRuntime(startup, *config, os.Getenv("ANET_DATABASE_URL"))
	stop()
	if err != nil {
		return err
	}
	defer db.Close()
	defer server.Close()
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	log.Printf("LISTEN %s", listener.Addr())
	err = server.ServeTLS(listener, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
