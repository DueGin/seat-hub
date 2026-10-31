package selfcheck

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

// Engine gathers slot snapshots from vm2api and records results.
// PLACEHOLDER: the self-check worker replaces this file (keep the exported
// names and signatures: NewEngine, *Engine, Check, Record).
type Engine struct {
	client vm2api.Client
	db     *sql.DB
	now    func() time.Time
}

func NewEngine(client vm2api.Client, db *sql.DB) *Engine {
	return &Engine{client: client, db: db, now: time.Now}
}

var errNotImplemented = errors.New("selfcheck engine not implemented")

func (e *Engine) Check(ctx context.Context, vmID string, opts Options) (Result, error) {
	return Result{}, errNotImplemented
}

func (e *Engine) Record(ctx context.Context, resourceID int64, attempt int, r Result) error {
	return errNotImplemented
}

// Evaluate applies every rule to a snapshot. PLACEHOLDER.
func Evaluate(in Input) Result { return Result{VMID: in.VM.ID} }

var _ Checker = (*Engine)(nil)
