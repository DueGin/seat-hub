package app

import (
	"context"
	"errors"
	"time"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/buildinfo"
	"github.com/DueGin/seat-hub/internal/update"
)

// systemAdapter serves api.SystemService from the in-site updater.
type systemAdapter struct {
	m     *update.Manager
	drain int // seconds a restart waits for in-flight requests
}

func (s *systemAdapter) UpdateStatus(ctx context.Context, refresh bool) api.UpdateStatusDTO {
	status := s.m.Status(ctx, refresh)
	info := buildinfo.Current()
	out := api.UpdateStatusDTO{
		Current: status.Current, Commit: info.Commit, BuildTime: info.BuildTime,
		Supervisor: status.Supervisor, DrainSeconds: s.drain, CanUpdate: status.CanUpdate, Reason: status.Reason,
		Changelog: make([]api.ReleaseDTO, 0, len(status.Changelog)), UpdateAvailable: status.UpdateAvailable,
		CheckError: status.CheckError, Pending: status.Pending,
	}
	if !status.CheckedAt.IsZero() {
		checked := status.CheckedAt.UTC()
		out.CheckedAt = &checked
	}
	if status.Latest != nil {
		latest := toReleaseDTO(*status.Latest)
		out.Latest = &latest
	}
	for _, release := range status.Changelog {
		out.Changelog = append(out.Changelog, toReleaseDTO(release))
	}
	if status.Job != nil {
		job := toJobDTO(*status.Job)
		out.Job = &job
	}
	if status.Rollback != nil {
		out.Rollback = &api.InstalledDTO{Version: status.Rollback.Version, Source: status.Rollback.Source}
	}
	if status.LastFailure != nil {
		out.LastFailure = &api.UpdateFailureDTO{Version: status.LastFailure.Version, Error: status.LastFailure.Error, At: status.LastFailure.At}
	}
	return out
}

func (s *systemAdapter) ApplyUpdate(ctx context.Context, version string) (api.UpdateJobDTO, error) {
	job, err := s.m.Apply(ctx, version)
	if err != nil {
		return api.UpdateJobDTO{}, mapUpdateErr(err)
	}
	return toJobDTO(job), nil
}

func (s *systemAdapter) Rollback(ctx context.Context) (api.UpdateJobDTO, error) {
	job, err := s.m.Rollback(ctx)
	if err != nil {
		return api.UpdateJobDTO{}, mapUpdateErr(err)
	}
	return toJobDTO(job), nil
}

func toReleaseDTO(release update.Release) api.ReleaseDTO {
	return api.ReleaseDTO{Version: release.Version, Name: release.Name, Notes: release.Notes, URL: release.URL, PublishedAt: release.PublishedAt.UTC()}
}

func toJobDTO(job update.Job) api.UpdateJobDTO {
	return api.UpdateJobDTO{Kind: job.Kind, Version: job.Version, Stage: job.Stage, Done: job.Done, Total: job.Total, Error: job.Error, StartedAt: job.StartedAt.UTC().Truncate(time.Second)}
}

func mapUpdateErr(err error) error {
	var unavailable *update.UnavailableError
	switch {
	case errors.As(err, &unavailable):
		return &api.UserError{Status: 409, Code: "update_unavailable", Message: unavailable.Reason}
	case errors.Is(err, update.ErrBusy):
		return api.Err(err, 409, "update_in_progress", "已有更新或回滚正在进行，请稍候")
	case errors.Is(err, update.ErrPending):
		return api.Err(err, 409, "update_pending", "新版本刚刚启动，正在确认运行正常，请约 30 秒后再试")
	case errors.Is(err, update.ErrInvalidVersion):
		return api.Err(err, 422, "invalid_version", "版本号格式不正确")
	case errors.Is(err, update.ErrNotLatest):
		return api.Err(err, 409, "version_not_latest", "只能更新到最新发布的版本，请刷新后重试")
	case errors.Is(err, update.ErrNotNewer):
		return api.Err(err, 409, "version_not_newer", "该版本不比当前运行的版本新")
	case errors.Is(err, update.ErrNoRollback):
		return api.Err(err, 409, "no_rollback", "没有可以回滚到的上一个版本")
	case errors.Is(err, update.ErrUpstream):
		return api.Err(err, 502, "update_source_unavailable", "无法连接 GitHub 检查或下载新版本，请稍后重试")
	}
	return err
}
