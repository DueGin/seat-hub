package selfcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

// Engine gathers slot snapshots from vm2api and records results.
type Engine struct {
	client vm2api.Client
	db     *sql.DB
	now    func() time.Time
}

func NewEngine(client vm2api.Client, db *sql.DB) *Engine {
	return &Engine{client: client, db: db, now: time.Now}
}

func (e *Engine) Check(ctx context.Context, vmID string, opts Options) (Result, error) {
	vms, err := e.client.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		return Result{}, fmt.Errorf("list vms: %w", err)
	}
	in := Input{Rules: opts.Rules, CheckAccount: opts.CheckAccount}
	found := false
	for _, vm := range vms {
		if vm.ID == vmID {
			in.VM = vm
			found = true
		} else {
			in.Peers = append(in.Peers, vm)
		}
	}
	if !found {
		return Result{}, fmt.Errorf("vm %q not found", vmID)
	}
	fingerprint, err := e.client.CollectIdentity(ctx, vmID)
	if err != nil {
		in.CollectErr = err
	} else {
		in.VM.Fingerprint = fingerprint
	}
	in.Probe, in.ProbeErr = e.client.ProbeExit(ctx, vmID)
	in.Now = e.now()
	in.PeerExitIPs = e.peerExitIPs(ctx, in.Now)
	return Evaluate(in), nil
}

func (e *Engine) Record(ctx context.Context, resourceID int64, attempt int, r Result) error {
	if e.db == nil {
		return fmt.Errorf("database unavailable")
	}
	checks, err := json.Marshal(r.Checks)
	if err != nil {
		return fmt.Errorf("encode checks: %w", err)
	}
	var resource any
	if resourceID != 0 {
		resource = resourceID
	}
	_, err = e.db.ExecContext(ctx, `INSERT INTO slot_checks
		(resource_id, vm_id, attempt, passed, checks, exit_ip, proxy_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, resource, r.VMID, attempt, r.Passed,
		string(checks), r.ExitIP, r.ProxyID, r.CheckedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("record slot check: %w", err)
	}
	return nil
}

func (e *Engine) peerExitIPs(ctx context.Context, now time.Time) map[string]string {
	if e.db == nil {
		return nil
	}
	rows, err := e.db.QueryContext(ctx, `SELECT vm_id, exit_ip FROM slot_checks
		WHERE exit_ip <> '' AND created_at >= ? AND created_at <= ?
		ORDER BY created_at DESC, id DESC`, now.Add(-24*time.Hour).UTC().Format(time.RFC3339),
		now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil
	}
	defer rows.Close()
	ips := make(map[string]string)
	for rows.Next() {
		var vmID, exitIP string
		if err := rows.Scan(&vmID, &exitIP); err != nil {
			return nil
		}
		if _, seen := ips[vmID]; !seen {
			ips[vmID] = exitIP
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return ips
}

var _ Checker = (*Engine)(nil)
