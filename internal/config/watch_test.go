package config

import (
	"os"
	"testing"
	"time"
)

func TestWatcherReload(t *testing.T) {
	path := writeTemp(t, "main:\n  baseurl: \"https://a/v1\"\n  api_key: \"k\"\n  format: openai\n  model: \"m1\"\n")
	w, err := NewWatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Get().Main().Model != "m1" {
		t.Fatal("initial model mismatch")
	}
	// 模拟编辑器原子写（rename 保存）
	if err := os.WriteFile(path, []byte("main:\n  baseurl: \"https://a/v1\"\n  api_key: \"k\"\n  format: openai\n  model: \"m2\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.Get().Main().Model == "m2" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("config did not reload after rename save")
}

func TestWatcherKeepsOldOnInvalid(t *testing.T) {
	path := writeTemp(t, "main:\n  baseurl: \"https://a/v1\"\n  api_key: \"k\"\n  format: openai\n  model: \"m1\"\n")
	w, err := NewWatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := os.WriteFile(path, []byte("broken: [yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // 给 watch 循环一个处理窗口
	if got := w.Get().Main().Model; got != "m1" {
		t.Fatalf("old config replaced with invalid one: %s", got)
	}
}
