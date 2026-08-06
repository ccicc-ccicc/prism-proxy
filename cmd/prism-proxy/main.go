package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"prism-proxy/internal/config"
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
			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("invalid config (fail fast): %w", err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "handler wiring in later tasks", http.StatusNotImplemented)
			})
			return http.ListenAndServe(cfg.Server.Listen, mux)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "prism-proxy.yaml", "path to config file")
	return cmd
}
