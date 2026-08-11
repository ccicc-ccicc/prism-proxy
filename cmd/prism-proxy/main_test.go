package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigPath_Explicit(t *testing.T) {
	got, err := resolveConfigPath("/tmp/x.yaml", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/x.yaml" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveConfigPath_Default(t *testing.T) {
	got, err := resolveConfigPath("~/.prism-proxy/settings.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".prism-proxy", "settings.yaml")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
