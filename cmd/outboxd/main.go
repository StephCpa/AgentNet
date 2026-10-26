// outboxd runs the single-domain local notification delivery worker.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentnet/agentnet/services/delivery"
	"github.com/agentnet/agentnet/services/tasktree"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dsn, domain := os.Getenv("ANET_DATABASE_URL"), os.Getenv("ANET_DOMAIN")
	if dsn == "" || domain == "" {
		log.Fatal("ANET_DATABASE_URL and ANET_DOMAIN are required")
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	q, err := delivery.New(db, delivery.Config{Domain: domain, Destination: "local-control"})
	if err != nil {
		log.Fatal(err)
	}
	inbox, err := delivery.NewInbox(db, domain, "local-control")
	if err != nil {
		log.Fatal(err)
	}
	log.Print("local notification worker started")
	tree, err := tasktree.New(db, domain)
	if err != nil {
		log.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- q.Run(ctx, inbox) }()
	go func() { results <- tree.Run(ctx) }()
	err = <-results
	cancel()
	other := <-results
	if err == nil || errors.Is(err, context.Canceled) {
		err = other
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
