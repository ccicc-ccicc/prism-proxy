package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"prism-proxy/internal/config"
	"prism-proxy/internal/server"
	"prism-proxy/internal/upstream"
)

// version 为编译期注入的版本号，默认 dev；构建时通过
// -ldflags "-X main.version=$(VERSION)" 覆盖
var version = "dev"

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
			watcher, err := config.NewWatcher(configPath)
			if err != nil {
				return fmt.Errorf("invalid config (fail fast): %w", err)
			}
			defer watcher.Close()
			srv := server.New(watcher, upstream.NewClient(), slog.Default())
			logger := slog.Default()
			logger.Info("prism-proxy listening", "addr", watcher.Get().Server.Listen, "config", configPath)
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
	cmd.Flags().StringVar(&configPath, "config", "prism-proxy.yaml", "path to config file")
	return cmd
}
