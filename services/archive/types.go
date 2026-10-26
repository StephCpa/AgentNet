// Package archive maintains cold event/audit partitions and expired delivery data.
// It is an operator service, not a worker-facing API; its DB role owns partitions.
package archive

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrBusy = errors.New("archive maintenance already running")

type Object struct {
	Body    io.ReadCloser
	Version string
}
type ObjectStore interface {
	ID() string
	// Put must be create-only. If the key exists, return its current version;
	// the caller reads and validates the complete bytes before trusting it.
	Put(context.Context, string, io.ReadSeeker, int64) (string, error)
	Open(context.Context, string, string) (Object, error)
}
type Config struct {
	WorkDir       string
	ChunkRows     int
	MaxChunkBytes int64
}
type Service struct {
	db      *pgxpool.Pool
	objects ObjectStore
	cfg     Config
}

func New(db *pgxpool.Pool, objects ObjectStore, cfg Config) (*Service, error) {
	if db == nil || objects == nil || objects.ID() == "" {
		return nil, errors.New("archive database and object store required")
	}
	if cfg.ChunkRows == 0 {
		cfg.ChunkRows = 10000
	}
	if cfg.MaxChunkBytes == 0 {
		cfg.MaxChunkBytes = 64 << 20
	}
	info, err := os.Stat(cfg.WorkDir)
	if err != nil || !info.IsDir() || cfg.ChunkRows < 1 || cfg.ChunkRows > 100000 || cfg.MaxChunkBytes < 1024 || cfg.MaxChunkBytes > 256<<20 {
		return nil, errors.New("invalid archive work directory or chunk limits")
	}
	return &Service{db, objects, cfg}, nil
}

type Job struct {
	ID              string    `json:"job_id"`
	Parent          string    `json:"parent"`
	Partition       string    `json:"partition"`
	OID             int64     `json:"-"`
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	Store           string    `json:"store"`
	State           string    `json:"-"`
	ManifestKey     string    `json:"-"`
	ManifestDigest  string    `json:"-"`
	ManifestVersion string    `json:"-"`
}
type Chunk struct {
	Ordinal int       `json:"ordinal"`
	Key     string    `json:"key"`
	Version string    `json:"version"`
	Digest  string    `json:"digest"`
	Size    int64     `json:"size"`
	Rows    int64     `json:"rows"`
	LastAt  time.Time `json:"last_at"`
	LastSeq int64     `json:"last_seq"`
}

// JSON preserves every source column (including nulls and future schema fields).
// Typed columns support Parquet filtering and the compact task lookup index.
type Record struct {
	Table    string `parquet:"source_table"`
	AtMicros int64  `parquet:"at_micros"`
	Seq      int64  `parquet:"seq"`
	Domain   string `parquet:"domain"`
	Task     string `parquet:"task"`
	JSON     string `parquet:"record_json"`
}
type Manifest struct {
	Format string  `json:"format"`
	Job    Job     `json:"job"`
	Chunks []Chunk `json:"chunks"`
	Rows   int64   `json:"rows"`
}
