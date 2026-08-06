package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"prism-proxy/internal/config"
	"prism-proxy/internal/server"
	"prism-proxy/internal/upstream"
)

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
			fmt.Println("prism-proxy 0.1.0")
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
			return http.ListenAndServe(watcher.Get().Server.Listen, srv)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "prism-proxy.yaml", "path to config file")
	return cmd
}
