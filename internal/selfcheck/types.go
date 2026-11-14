// Package selfcheck decides whether a slot may be signed into.
//
// The rule is "one proxy IP, one account, one slot": a slot must leave through
// its own dedicated exit, carry a fingerprint no other slot shares, and hold an
// account no other slot holds. See docs/SELFCHECK.md.
//
// The package is split in two:
//   - Evaluate is pure: it turns an Input snapshot into a Result. All rule
//     logic lives here and is unit-tested without I/O.
//   - Engine gathers the snapshot from vm2api (identity, exit probe, peer
//     slots) and records results in slot_checks.
//
// Remediation (reset / re-proxy / retry budget) belongs to the caller
// (internal/provision); this package only reports what is wrong and which
// remedy would address it.
package selfcheck

import (
	"context"
	"time"

	"github.com/DueGin/seat-hub/internal/vm2api"
)

// Code identifies one rule. Values are stable: they are stored in slot_checks
// and shown in the admin console.
type Code string

const (
	ProxyBound         Code = "proxy_bound"         // bound, and dedicated socks5 (not shared local egress)
	ProxyHealthy       Code = "proxy_healthy"       // enabled, not dead/fail, not blocked
	ProxyExclusive     Code = "proxy_exclusive"     // no other slot bound to the same proxy id
	ExitProbe          Code = "exit_probe"          // vm2api exit probe succeeded and returned an IP
	ExitIPUnique       Code = "exit_ip_unique"      // no other slot leaves through the same IP
	ExitGeoTimezone    Code = "exit_geo_timezone"   // slot timezone matches the exit's geo timezone
	FingerprintPresent Code = "fingerprint_present" // device_id, guest_machine_id, hostname all set
	FingerprintUnique  Code = "fingerprint_unique"  // none of those keys is shared with another slot
	AccountUnique      Code = "account_unique"      // the signed-in account is on no other slot
)

// Remedy is what fixes a failed rule.
type Remedy string

const (
	RemedyNone    Remedy = ""        // passed
	RemedyReset   Remedy = "reset"   // regenerate the fingerprint (vm2api reset mode=traits)
	RemedyReproxy Remedy = "reproxy" // unbind (and possibly disable) the proxy, allocate a new one
	RemedyManual  Remedy = "manual"  // automation cannot fix it (e.g. duplicate account)
)

// remedyRank orders remedies so a Result can report the strongest one needed.
var remedyRank = map[Remedy]int{RemedyNone: 0, RemedyReset: 1, RemedyReproxy: 2, RemedyManual: 3}

// Check is the outcome of one rule. Message is user-safe Chinese text;
// Detail is for the admin console only (may contain IPs / ids).
type Check struct {
	Code    Code   `json:"code"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Remedy  Remedy `json:"remedy,omitempty"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// Result is one complete self-check of a slot.
type Result struct {
	VMID      string    `json:"vm_id"`
	Passed    bool      `json:"passed"`
	Checks    []Check   `json:"checks"`
	ExitIP    string    `json:"exit_ip"`
	ProxyID   string    `json:"proxy_id"`
	CheckedAt time.Time `json:"checked_at"`
}

// Failed returns the checks that did not pass (skipped checks are not failures).
func (r Result) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK && !c.Skipped {
			out = append(out, c)
		}
	}
	return out
}

// Remedy returns the strongest remedy any failed check needs.
func (r Result) Remedy() Remedy {
	best := RemedyNone
	for _, c := range r.Failed() {
		if remedyRank[c.Remedy] > remedyRank[best] {
			best = c.Remedy
		}
	}
	return best
}

// Needs reports whether any failed check asks for the given remedy.
func (r Result) Needs(remedy Remedy) bool {
	for _, c := range r.Failed() {
		if c.Remedy == remedy {
			return true
		}
	}
	return false
}

// Rules are the admin-tunable switches (store.Site selfcheck_*).
type Rules struct {
	RequireDedicatedProxy bool
	RequireTimezoneMatch  bool
}

// Input is everything Evaluate needs. Gathering it is the Engine's job.
type Input struct {
	// VM is the target slot as vm2api lists it. Its Fingerprint is replaced by
	// the freshly collected one when collection succeeded.
	VM vm2api.VM
	// CollectErr is set when CollectIdentity failed; the listed fingerprint is
	// then the only evidence.
	CollectErr error
	Probe      vm2api.ExitProbe
	ProbeErr   error
	// Peers are every other slot vm2api knows about (all owners).
	Peers []vm2api.VM
	// PeerExitIPs maps a peer VM id to the exit IP last observed for it by a
	// recorded self-check; it complements Peer.Proxy.GeoIP, which is only the
	// pool's cached geo and may be stale.
	PeerExitIPs map[string]string
	// CheckAccount enables account_unique (only meaningful once signed in).
	CheckAccount bool
	Rules        Rules
	Now          time.Time
}

// Options select which rules apply to one run.
type Options struct {
	Rules Rules
	// CheckAccount enables account_unique; set it once the slot is signed in
	// (post-exchange, rental pool admission, periodic sweep).
	CheckAccount bool
}

// Checker is what callers depend on; *Engine implements it.
type Checker interface {
	// Check gathers a fresh snapshot of the slot and evaluates every rule. It
	// returns an error only when the snapshot itself could not be taken (e.g.
	// vm2api unreachable); a slot that fails rules is a Result with Passed=false.
	Check(ctx context.Context, vmID string, opts Options) (Result, error)
	// Record persists a result to slot_checks. resourceID 0 means "not tied to
	// a portal resource" (rental-pool admission).
	Record(ctx context.Context, resourceID int64, attempt int, r Result) error
}
