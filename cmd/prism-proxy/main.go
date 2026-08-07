package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"prism-proxy/internal/config"
	"prism-proxy/internal/server"
	"prism-proxy/internal/trafficlog"
	"prism-proxy/internal/upstream"
)

// version 为编译期注入的版本号，默认 dev；构建时通过
// -ldflags "-X main.version=$(VERSION)" 覆盖
var version = "dev"

// defaultConfigFlag 为 --config 标志的默认字面值（help 展示用）。
const defaultConfigFlag = "~/.prism-proxy/settings.yaml"

// resolveConfigPath 解析配置路径：显式指定则原样返回；
// 缺省时展开 ~/.prism-proxy/settings.yaml 为绝对路径。
func resolveConfigPath(flagValue string, changed bool) (string, error) {
	if changed {
		return flagValue, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".prism-proxy", "settings.yaml"), nil
}

func main() {
	root := &cobra.Command{
		Use:   "prism-proxy",
		Short: "Local LLM API proxy: OpenAI <-> Claude protocol gateway",
	}
	root.AddCommand(versionCmd())
	root.AddCommand(serveCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("prism-proxy " + version)
		},
	}
}

func serveCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the proxy server",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(configPath, cmd.Flags().Changed("config"))
			if err != nil {
				return err
			}
			watcher, err := config.NewWatcher(path)
			if err != nil {
				if !cmd.Flags().Changed("config") && errors.Is(err, fs.ErrNotExist) {
					// 缺省路径不存在：提示默认路径与旧默认迁移
					return fmt.Errorf("config not found: %s; default config path is ~/.prism-proxy/settings.yaml; if you relied on the old default ./prism-proxy.yaml, pass --config prism-proxy.yaml explicitly", path)
				}
				return fmt.Errorf("invalid config (fail fast): %w", err)
			}
			defer watcher.Close()
			// 内容日志：启动时 enabled 则构造 TrafficLog（dir/max_files 变更需重启）
			var traffic *trafficlog.TrafficLog
			if cfg := watcher.Get(); cfg.Logging.Enabled {
				traffic, err = trafficlog.New(cfg.Logging.Dir, cfg.Logging.MaxBackups())
				if err != nil {
					return fmt.Errorf("init traffic log: %w", err)
				}
				defer traffic.Close()
			}
			srv := server.New(watcher, upstream.NewClient(), slog.Default())
			srv.SetTrafficLog(traffic)
			logger := slog.Default()
			logger.Info("prism-proxy listening", "addr", watcher.Get().Server.Listen, "config", path)
			httpServer := &http.Server{
				Addr:              watcher.Get().Server.Listen,
				Handler:           srv,
				ReadHeaderTimeout: 30 * time.Second, // 慢速/恶意客户端不长期占用连接
				IdleTimeout:       120 * time.Second,
			}
			// 优雅关闭：Ctrl-C / SIGTERM → 停止接收新请求并等待在途请求完成
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = httpServer.Shutdown(shutdownCtx)
			}()
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultConfigFlag, "path to config file")
	return cmd
}
