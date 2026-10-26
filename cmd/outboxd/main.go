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
	if err = q.Run(ctx, inbox); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
