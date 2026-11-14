package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// BinaryName is the executable in release assets and in the update directory.
const BinaryName = "seat-hub"

// Status of the installed update.
const (
	StatusActive  = "active"  // running and confirmed healthy
	StatusPending = "pending" // installed, waiting for the new process to prove itself
)

// Source of an installed version.
const (
	SourceDownload = "download" // a binary this package downloaded into the update directory
	SourceImage    = "image"    // the binary the process was deployed with (Docker image / systemd install)
)

// Installed is one version the launcher can run.
type Installed struct {
	Version     string `json:"version"`
	SHA256      string `json:"sha256,omitempty"` // empty for SourceImage
	Source      string `json:"source"`
	InstalledAt string `json:"installed_at,omitempty"`
}

// Failure records why the last update was abandoned.
type Failure struct {
	Version string `json:"version"`
	Error   string `json:"error"`
	At      string `json:"at"`
}

// State is persisted as state.json in the update directory. Only the launcher
// and the Manager write it; both run inside the same single process tree.
type State struct {
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	// Current is the downloaded version to run; nil means "run the deployed
	// binary".
	Current *Installed `json:"current,omitempty"`
	// Previous is what Current replaced: the rollback target.
	Previous    *Installed `json:"previous,omitempty"`
	LastFailure *Failure   `json:"last_failure,omitempty"`
	UpdatedAt   string     `json:"updated_at"`
}

type layout struct{ dir string }

func (l layout) current() string  { return filepath.Join(l.dir, BinaryName) }
func (l layout) previous() string { return filepath.Join(l.dir, BinaryName+".prev") }
func (l layout) state() string    { return filepath.Join(l.dir, "state.json") }

// loadState returns the zero State (and no error) when nothing was installed.
func loadState(l layout) (State, error) {
	raw, err := os.ReadFile(l.state())
	if errors.Is(err, fs.ErrNotExist) {
		return State{Status: StatusActive}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read update state: %w", err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, fmt.Errorf("decode update state: %w", err)
	}
	if state.Status == "" {
		state.Status = StatusActive
	}
	return state, nil
}

// saveState writes atomically: a crash mid-write leaves the old file intact.
func saveState(l layout, state State, now time.Time) error {
	state.UpdatedAt = now.UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode update state: %w", err)
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return fmt.Errorf("create update dir: %w", err)
	}
	tmp, err := os.CreateTemp(l.dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("create update state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write update state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync update state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close update state: %w", err)
	}
	if err := os.Rename(tmp.Name(), l.state()); err != nil {
		return fmt.Errorf("replace update state: %w", err)
	}
	return nil
}

// fileSHA256 hashes a file; an unreadable file has no hash.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// intact reports whether path holds exactly the binary that was installed.
func intact(path, sum string) bool {
	if sum == "" {
		return false
	}
	got, err := fileSHA256(path)
	return err == nil && got == sum
}
