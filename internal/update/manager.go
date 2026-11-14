package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Errors a caller maps to user-facing responses.
var (
	ErrUnavailable    = errors.New("in-site update unavailable")
	ErrBusy           = errors.New("an update is already in progress")
	ErrInvalidVersion = errors.New("invalid version")
	ErrNotLatest      = errors.New("version is not the latest release")
	ErrNotNewer       = errors.New("version is not newer than the running one")
	ErrPending        = errors.New("the running update is still being confirmed")
	ErrNoRollback     = errors.New("nothing to roll back to")
	ErrUpstream       = errUpstream
)

// UnavailableError explains why this deployment cannot update itself.
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string { return "in-site update unavailable: " + e.Reason }
func (e *UnavailableError) Unwrap() error { return ErrUnavailable }

// Job stages.
const (
	StageDownloading = "downloading"
	StageVerifying   = "verifying"
	StageInstalling  = "installing"
	StageRestarting  = "restarting"
	StageFailed      = "failed"
)

const (
	assetLimit     = 256 << 20
	checksumsLimit = 64 << 10
	versionTimeout = 20 * time.Second
	jobTimeout     = 15 * time.Minute
)

// Config wires a Manager. Restart must make the process drain and exit so its
// supervisor (Docker restart policy, systemd) starts it again.
type Config struct {
	Repo       string // owner/name on GitHub
	APIBase    string // https://api.github.com, overridable for mirrors and tests
	Token      string // optional GitHub token (raises the API rate limit)
	Dir        string
	Current    string // version of the running process
	GOOS       string
	GOARCH     string
	Supervisor string // "docker" | "systemd" | "" (not supervised)
	HTTP       *http.Client
	Restart    func()
	Now        func() time.Time
	Log        *slog.Logger
	CacheTTL   time.Duration
}

// Job is the update or rollback in flight.
type Job struct {
	Kind      string    `json:"kind"` // update | rollback
	Version   string    `json:"version"`
	Stage     string    `json:"stage"`
	Done      int64     `json:"done"`
	Total     int64     `json:"total"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// Status is everything the admin page shows.
type Status struct {
	Current         string     `json:"current"`
	Supervisor      string     `json:"supervisor"`
	CanUpdate       bool       `json:"can_update"`
	Reason          string     `json:"reason,omitempty"`
	Latest          *Release   `json:"latest"`
	Changelog       []Release  `json:"changelog"`
	UpdateAvailable bool       `json:"update_available"`
	CheckedAt       time.Time  `json:"checked_at"`
	CheckError      string     `json:"check_error,omitempty"`
	Job             *Job       `json:"job"`
	Pending         bool       `json:"pending"`
	Rollback        *Installed `json:"rollback"`
	LastFailure     *Failure   `json:"last_failure"`
}

type Manager struct {
	cfg Config
	l   layout

	mu        sync.Mutex
	releases  []Release
	checkedAt time.Time
	checkErr  error
	job       *Job
}

func New(cfg Config) *Manager {
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 10 * time.Minute
	}
	if cfg.Restart == nil {
		cfg.Restart = func() {}
	}
	return &Manager{cfg: cfg, l: layout{dir: cfg.Dir}}
}

// DetectSupervisor reports who restarts this process: SEAT_HUB_SUPERVISOR
// (set by the Docker image and the systemd unit), or systemd's INVOCATION_ID.
// A bare process has none, and must not update itself: it would just exit.
func DetectSupervisor() string {
	switch value := strings.ToLower(strings.TrimSpace(os.Getenv("SEAT_HUB_SUPERVISOR"))); value {
	case "docker", "systemd":
		return value
	case "none", "off":
		return ""
	}
	if os.Getenv("INVOCATION_ID") != "" {
		return "systemd"
	}
	return ""
}

// Status reports versions and progress. refresh forces a release check (at
// most once every 15 seconds); otherwise a cached check is reused.
func (m *Manager) Status(ctx context.Context, refresh bool) Status {
	releases, checkedAt, checkErr := m.cachedReleases(ctx, refresh)
	state, stateErr := loadState(m.l)
	status := Status{Current: m.cfg.Current, Supervisor: m.cfg.Supervisor, Changelog: []Release{}, CheckedAt: checkedAt}
	if checkErr != nil {
		status.CheckError = userText(checkErr)
	}
	if len(releases) > 0 {
		latest := releases[0]
		status.Latest = &latest
		status.UpdateAvailable = Newer(latest.Version, m.cfg.Current)
		for _, release := range releases {
			if Newer(release.Version, m.cfg.Current) && len(status.Changelog) < 10 {
				status.Changelog = append(status.Changelog, release)
			}
		}
	}
	reason := m.unavailableReason()
	if reason == "" && status.UpdateAvailable && status.Latest.assets[AssetName(m.cfg.GOOS, m.cfg.GOARCH)] == "" {
		reason = fmt.Sprintf("新版本没有提供 %s/%s 平台的程序", m.cfg.GOOS, m.cfg.GOARCH)
	}
	status.CanUpdate, status.Reason = reason == "", reason
	if stateErr == nil {
		runningDownload := state.Current != nil && state.Current.Version == m.cfg.Current
		status.Pending = runningDownload && state.Status == StatusPending
		if runningDownload && state.Previous != nil {
			previous := *state.Previous
			if previous.Source == SourceImage || intact(m.l.previous(), previous.SHA256) {
				status.Rollback = &previous
			}
		}
		status.LastFailure = state.LastFailure
	}
	m.mu.Lock()
	if m.job != nil {
		job := *m.job
		status.Job = &job
	}
	m.mu.Unlock()
	return status
}

func (m *Manager) cachedReleases(ctx context.Context, refresh bool) ([]Release, time.Time, error) {
	now := m.cfg.Now()
	m.mu.Lock()
	age := now.Sub(m.checkedAt)
	if !m.checkedAt.IsZero() && (age < m.cfg.CacheTTL && !refresh || age < 15*time.Second) {
		releases, checkedAt, err := m.releases, m.checkedAt, m.checkErr
		m.mu.Unlock()
		return releases, checkedAt, err
	}
	m.mu.Unlock()
	releases, err := m.fetchReleases(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkedAt, m.checkErr = now, err
	if err == nil {
		m.releases = releases
	}
	return m.releases, m.checkedAt, m.checkErr
}

// unavailableReason is "" when this deployment can update and restart itself.
func (m *Manager) unavailableReason() string {
	if !ValidTag(m.cfg.Current) {
		return "开发版本不支持在线更新，请使用正式发布的版本"
	}
	if m.cfg.Supervisor == "" {
		return "当前进程不在 Docker 或 systemd 托管下运行，更新后无法自动重启"
	}
	if err := os.MkdirAll(m.cfg.Dir, 0o755); err != nil {
		return fmt.Sprintf("更新目录 %s 无法创建", m.cfg.Dir)
	}
	probe, err := os.CreateTemp(m.cfg.Dir, ".probe-*")
	if err != nil {
		return fmt.Sprintf("更新目录 %s 不可写", m.cfg.Dir)
	}
	probe.Close()
	os.Remove(probe.Name())
	return ""
}

// Apply starts updating to version, which must be the latest release and
// newer than the running one. It returns at once; progress shows in Status.
func (m *Manager) Apply(ctx context.Context, version string) (Job, error) {
	if reason := m.unavailableReason(); reason != "" {
		return Job{}, &UnavailableError{Reason: reason}
	}
	if !ValidTag(version) {
		return Job{}, ErrInvalidVersion
	}
	if !Newer(version, m.cfg.Current) {
		return Job{}, ErrNotNewer
	}
	if err := m.checkIdle(); err != nil {
		return Job{}, err
	}
	releases, _, err := m.cachedReleases(ctx, true)
	if err != nil && len(releases) == 0 {
		return Job{}, err
	}
	if len(releases) == 0 || releases[0].Version != version {
		return Job{}, ErrNotLatest
	}
	release := releases[0]
	asset := AssetName(m.cfg.GOOS, m.cfg.GOARCH)
	assetURL, sumsURL := release.assets[asset], release.assets[ChecksumsAsset]
	if assetURL == "" || sumsURL == "" {
		return Job{}, &UnavailableError{Reason: fmt.Sprintf("版本 %s 缺少 %s 或 %s", version, asset, ChecksumsAsset)}
	}
	job, err := m.start(Job{Kind: "update", Version: version, Stage: StageDownloading})
	if err != nil {
		return Job{}, err
	}
	go m.runUpdate(release, asset, assetURL, sumsURL)
	return job, nil
}

// Rollback restores the version the running update replaced and restarts.
func (m *Manager) Rollback(ctx context.Context) (Job, error) {
	if reason := m.unavailableReason(); reason != "" {
		return Job{}, &UnavailableError{Reason: reason}
	}
	state, err := loadState(m.l)
	if err != nil {
		return Job{}, err
	}
	if state.Current == nil || state.Current.Version != m.cfg.Current || state.Previous == nil {
		return Job{}, ErrNoRollback
	}
	previous := *state.Previous
	if previous.Source == SourceDownload && !intact(m.l.previous(), previous.SHA256) {
		return Job{}, ErrNoRollback
	}
	job, err := m.start(Job{Kind: "rollback", Version: previous.Version, Stage: StageInstalling})
	if err != nil {
		return Job{}, err
	}
	if previous.Source == SourceDownload {
		err = os.Rename(m.l.previous(), m.l.current())
		state.Current = &previous
	} else {
		err = os.Remove(m.l.current())
		state.Current = nil
	}
	if err != nil && !os.IsNotExist(err) {
		m.fail(fmt.Errorf("恢复上一个版本失败: %w", err))
		return Job{}, err
	}
	state.Previous, state.Status, state.Attempts = nil, StatusActive, 0
	if err := saveState(m.l, state, m.cfg.Now()); err != nil {
		m.fail(err)
		return Job{}, err
	}
	m.cfg.Log.Info("在线更新：回滚并重启", "from", m.cfg.Current, "to", previous.Version)
	m.setStage(StageRestarting)
	go m.cfg.Restart()
	return job, nil
}

func (m *Manager) checkIdle() error {
	m.mu.Lock()
	busy := m.job != nil && m.job.Stage != StageFailed
	m.mu.Unlock()
	if busy {
		return ErrBusy
	}
	state, err := loadState(m.l)
	if err != nil {
		return err
	}
	if state.Status == StatusPending && state.Current != nil && state.Current.Version == m.cfg.Current {
		return ErrPending
	}
	return nil
}

func (m *Manager) start(job Job) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil && m.job.Stage != StageFailed {
		return Job{}, ErrBusy
	}
	job.StartedAt = m.cfg.Now()
	m.job = &job
	return job, nil
}

func (m *Manager) setStage(stage string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil {
		m.job.Stage = stage
	}
}

func (m *Manager) fail(err error) {
	m.cfg.Log.Error("在线更新失败", "err", err)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil {
		m.job.Stage, m.job.Error = StageFailed, userText(err)
	}
}

func (m *Manager) runUpdate(release Release, asset, assetURL, sumsURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	if err := m.install(ctx, release, asset, assetURL, sumsURL); err != nil {
		m.fail(err)
		return
	}
	m.cfg.Log.Info("在线更新：新版本已安装，准备重启", "from", m.cfg.Current, "to", release.Version)
	m.setStage(StageRestarting)
	m.cfg.Restart()
}

func (m *Manager) install(ctx context.Context, release Release, asset, assetURL, sumsURL string) error {
	var sums bytes.Buffer
	if err := m.download(ctx, sumsURL, &sums, checksumsLimit, nil); err != nil {
		return fmt.Errorf("下载校验文件失败: %w", err)
	}
	expected := parseChecksums(&sums)[asset]
	if expected == "" {
		return fmt.Errorf("校验文件中没有 %s", asset)
	}
	tmp, err := os.CreateTemp(m.cfg.Dir, ".download-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(tmp.Name())
		}
	}()
	hash := sha256.New()
	err = m.download(ctx, assetURL, io.MultiWriter(tmp, hash), assetLimit, func(done, total int64) {
		m.mu.Lock()
		if m.job != nil {
			m.job.Done, m.job.Total = done, total
		}
		m.mu.Unlock()
	})
	if syncErr := tmp.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("下载新版本失败: %w", err)
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if sum != expected {
		return errors.New("新版本的 SHA-256 与发布的校验值不一致，已放弃本次更新")
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return fmt.Errorf("设置可执行权限失败: %w", err)
	}

	m.setStage(StageVerifying)
	if err := m.verifyBinary(ctx, tmp.Name(), release.Version); err != nil {
		return err
	}

	m.setStage(StageInstalling)
	state, err := loadState(m.l)
	if err != nil {
		return err
	}
	var previous *Installed
	if state.Current != nil && state.Current.Version == m.cfg.Current {
		// Running a download: keep it as the rollback target. Renaming the
		// running executable is safe; the process keeps its inode.
		if err := os.Rename(m.l.current(), m.l.previous()); err != nil {
			return fmt.Errorf("保存当前版本失败: %w", err)
		}
		saved := *state.Current
		previous = &saved
	} else {
		_ = os.Remove(m.l.previous())
		previous = &Installed{Version: m.cfg.Current, Source: SourceImage}
	}
	if err := os.Rename(tmp.Name(), m.l.current()); err != nil {
		return fmt.Errorf("安装新版本失败: %w", err)
	}
	keep = true
	now := m.cfg.Now().UTC()
	next := State{
		Status:   StatusPending,
		Current:  &Installed{Version: release.Version, SHA256: sum, Source: SourceDownload, InstalledAt: now.Format(time.RFC3339)},
		Previous: previous,
	}
	return saveState(m.l, next, now)
}

// verifyBinary runs `<binary> version` and checks it reports the release:
// the file runs on this host and is the build it claims to be.
func (m *Manager) verifyBinary(ctx context.Context, path, want string) error {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, "version")
	command.Env = append(os.Environ(), ChildEnv+"=1")
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("新版本无法在当前系统运行: %w", err)
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &info); err != nil || info.Version != want {
		return fmt.Errorf("新版本自报的版本号（%q）与 %s 不一致", info.Version, want)
	}
	return nil
}

// userText keeps errors readable in the admin console.
func userText(err error) string {
	if errors.Is(err, errUpstream) {
		return "无法连接 GitHub 检查或下载新版本：" + strings.TrimPrefix(err.Error(), errUpstream.Error()+": ")
	}
	return err.Error()
}
