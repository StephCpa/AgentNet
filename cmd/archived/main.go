// archived performs recoverable Parquet/S3 archival and bounded expiry cleanup.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agentnet/agentnet/services/archive"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	path := flag.String("config", "", "archive JSON configuration")
	once := flag.Bool("once", false, "perform one maintenance cycle and exit")
	task := flag.String("task", "", "read a task's hot/detached/cold records instead of maintenance")
	domain := flag.String("domain", "", "required for task lookup")
	limit := flag.Int("limit", 10000, "maximum task lookup records (error on overflow)")
	flag.Parse()
	if *path == "" || os.Getenv("ANET_DATABASE_URL") == "" {
		return errors.New("-config and ANET_DATABASE_URL required")
	}
	var cfg struct {
		WorkDir       string           `json:"work_dir"`
		ChunkRows     int              `json:"chunk_rows"`
		MaxChunkBytes int64            `json:"max_chunk_bytes"`
		S3            archive.S3Config `json:"s3"`
	}
	f, err := os.Open(*path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	err = d.Decode(&cfg)
	var extra any
	if err == nil && d.Decode(&extra) != io.EOF {
		err = errors.New("trailing configuration data")
	}
	f.Close()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(cfg.WorkDir) {
		cfg.WorkDir = filepath.Join(filepath.Dir(*path), cfg.WorkDir)
	}
	objects, err := archive.NewS3(cfg.S3, os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN"), nil)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := pgxpool.New(ctx, os.Getenv("ANET_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	s, err := archive.New(db, objects, archive.Config{WorkDir: cfg.WorkDir, ChunkRows: cfg.ChunkRows, MaxChunkBytes: cfg.MaxChunkBytes})
	if err != nil {
		return err
	}
	if *task != "" {
		records, err := s.ReadTask(ctx, *domain, *task, *limit)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(records)
	}
	for {
		err = s.RunOnce(ctx)
		if *once {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		delay := 24 * time.Hour
		if err != nil {
			log.Printf("archive cycle failed: %v", err)
			delay = time.Minute
		} else {
			log.Print("archive cycle complete")
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
