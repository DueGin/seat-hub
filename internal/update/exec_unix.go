//go:build unix

package update

import (
	"log/slog"
	"os"
	"syscall"
	"time"
)

// MaybeExec replaces the current process with a downloaded update when one
// should run (see Resolve). It returns when the deployed binary should keep
// serving. Called first thing in main, before any state is opened.
func MaybeExec(deployed string, log *slog.Logger) {
	if os.Getenv(ChildEnv) == "1" {
		return
	}
	dir := Dir()
	path, err := Resolve(dir, deployed, time.Now())
	if err != nil {
		log.Warn("读取在线更新状态失败，继续运行当前版本", "dir", dir, "err", err)
		return
	}
	if path == "" {
		return
	}
	log.Info("启动已在线更新的版本", "path", path, "deployed", deployed)
	argv := append([]string{path}, os.Args[1:]...)
	env := append(os.Environ(), ChildEnv+"=1")
	if err := syscall.Exec(path, argv, env); err != nil {
		// The download is intact (Resolve checked) but cannot run here; the
		// attempt is already counted, so a few restarts roll it back.
		log.Error("无法启动已更新的版本，继续运行当前版本", "path", path, "err", err)
	}
}
