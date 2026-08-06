package config

import (
	"os"
	"path/filepath"
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
	// 模拟编辑器原子写：同目录写临时文件，再 os.Rename 原子替换（rename 保存）
	tmp := filepath.Join(filepath.Dir(path), "config.yaml.tmp")
	if err := os.WriteFile(tmp, []byte("main:\n  baseurl: \"https://a/v1\"\n  api_key: \"k\"\n  format: openai\n  model: \"m2\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
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

// TestWatcherTwoPhaseSave 覆盖 vim 风格 two-phase 保存：
// 先把旧文件 rename 走（watcher 短暂失效），再创建新文件。
// 验证 Add 失败后的后台重试能重新挂载并补加载新内容。
func TestWatcherTwoPhaseSave(t *testing.T) {
	path := writeTemp(t, "main:\n  baseurl: \"https://a/v1\"\n  api_key: \"k\"\n  format: openai\n  model: \"m1\"\n")
	w, err := NewWatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := os.Rename(path, path+"~"); err != nil {
		t.Fatal(err)
	}
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
	t.Fatal("config did not reload after two-phase save")
}
