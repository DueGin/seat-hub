package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DueGin/seat-hub/internal/app"
	"github.com/DueGin/seat-hub/internal/buildinfo"
	"github.com/DueGin/seat-hub/internal/config"
	"github.com/DueGin/seat-hub/internal/update"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// `seat-hub version` prints the build as JSON; the updater runs it to check
	// a download before installing it, so it must not depend on any config.
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		_ = json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
		return
	}
	version := buildinfo.Current().Version
	// Hand over to an in-site update if one is installed and newer than this
	// binary. Returns when this binary should serve.
	update.MaybeExec(version, logger)

	// A freshly installed update must prove itself. If it is not serving within
	// the deadline, exit non-zero: the supervisor restarts it, the launcher
	// counts the attempt and eventually rolls back.
	pending := update.NeedsConfirm(update.Dir(), version)
	var watchdog *time.Timer
	if pending {
		watchdog = time.AfterFunc(update.ConfirmDeadline, func() {
			logger.Error("新版本未能按时启动，退出以便回滚", "version", version)
			os.Exit(1)
		})
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Error("配置错误", "err", err)
		os.Exit(1)
	}
	a, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("启动失败", "err", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.BootstrapAdmin(ctx); err != nil {
		logger.Error("初始化管理员失败", "err", err)
		os.Exit(1)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() { errCh <- a.Start() }()
	logger.Info("版本信息", "version", version, "commit", buildinfo.Current().Commit, "supervisor", update.DetectSupervisor())

	if pending {
		go func() {
			time.Sleep(update.ConfirmAfter)
			if err := update.Confirm(update.Dir(), version, time.Now()); err != nil {
				logger.Error("确认新版本失败", "err", err)
				return
			}
			watchdog.Stop()
			logger.Info("新版本运行正常，在线更新完成", "version", version)
		}()
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务异常退出", "err", err)
			os.Exit(1)
		}
	case sig := <-stop:
		logger.Info("收到退出信号", "signal", sig.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.Close(shutdownCtx); err != nil {
			logger.Error("关闭失败", "err", err)
		}
	case <-a.RestartRequested():
		drain := time.Duration(cfg.UpdateDrainSeconds) * time.Second
		logger.Info("在线更新：停止接收新请求，等待进行中的请求结束后重启", "drain", drain.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), drain)
		if err := a.Close(shutdownCtx); err != nil {
			logger.Warn("重启前关闭未完全结束", "err", err)
		}
		cancel()
		os.Exit(update.RestartExitCode)
	}
}
