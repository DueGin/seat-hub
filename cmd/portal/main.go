package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DueGin/vm2api-portal/internal/app"
	"github.com/DueGin/vm2api-portal/internal/config"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

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
	}
}
