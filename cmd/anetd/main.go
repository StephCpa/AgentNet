package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentnet/agentnet/services/rpc"
	"google.golang.org/grpc"
)

func run() error {
	config := flag.String("config", "", "explicit server configuration JSON")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server, db, address, err := rpc.OpenRuntime(ctx, *config, os.Getenv("ANET_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	defer server.Stop()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			stopped := make(chan struct{})
			go func() { server.GracefulStop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				server.Stop()
			}
		case <-done:
		}
	}()
	log.Printf("LISTEN %s", listener.Addr())
	err = server.Serve(listener)
	if errors.Is(err, grpc.ErrServerStopped) || ctx.Err() != nil {
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
