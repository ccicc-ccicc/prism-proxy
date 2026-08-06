package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
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
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "config not loaded yet", http.StatusNotImplemented)
			})
			addr := ":8787"
			return http.ListenAndServe(addr, mux)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "prism-proxy.yaml", "path to config file")
	return cmd
}
