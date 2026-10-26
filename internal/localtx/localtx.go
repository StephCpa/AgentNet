// Package localtx defines the contract for trusted, compiled local handlers.
// Handlers must use only DB for effects: no network, separate connections,
// transaction-control SQL, or writes to AgentNet's control tables. This contract
// is a code-review boundary, not a sandbox for untrusted plugins.
package localtx

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var Invalid = errors.New("invalid local operation")

type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Request struct {
	Domain, TaskID, EffectID, Resource string
	Parameters                         json.RawMessage
}

type Result struct {
	Outcome string            `json:"outcome"`
	Payload json.RawMessage   `json:"payload"`
	Usage   map[string]string `json:"usage"`
}

type Handler interface {
	Validate(action, resource string, dimensions []string) error
	Execute(context.Context, DB, Request) (Result, error)
}
