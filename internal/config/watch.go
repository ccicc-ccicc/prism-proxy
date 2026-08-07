package config

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	cfg      atomic.Pointer[Config]
	path     string
	watcher  *fsnotify.Watcher
	readding atomic.Bool // 后台重挂任务去重
}

func NewWatcher(path string) (*Watcher, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := fw.Add(path); err != nil {
		fw.Close()
		return nil, err
	}
	w := &Watcher{path: path, watcher: fw}
	w.cfg.Store(cfg)
	go w.loop()
	return w, nil
}

func (w *Watcher) Get() *Config { return w.cfg.Load() }

func (w *Watcher) Close() error { return w.watcher.Close() }

func (w *Watcher) loop() {
	for {
		select {
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			// 编辑器原子写（rename/create/remove 替换）会丢失 inode watch，重新挂；
			// vim 风格 two-phase 保存（先 rename 走旧文件再创建）中文件会短暂缺失，
			// Add 失败则后台重试补挂并补加载，避免 watcher 永久失效。
			if ev.Op&(fsnotify.Rename|fsnotify.Create|fsnotify.Remove) != 0 {
				if err := w.watcher.Add(w.path); err != nil {
					w.scheduleReadd()
					continue // 文件此刻缺失，跳过必然失败的 Load；重试成功后补加载
				}
			}
			if ev.Op&(fsnotify.Write|fsnotify.Rename|fsnotify.Create|fsnotify.Remove) == 0 {
				continue
			}
			newCfg, err := Load(w.path)
			if err != nil {
				slog.Error("config reload failed, keeping old config", "err", err)
				continue
			}
			w.cfg.Store(newCfg)
			slog.Info("config reloaded", "path", w.path)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			slog.Error("config watcher error", "err", err)
		}
	}
}

// scheduleReadd 在后台重试重新挂载 watch：每 100ms 一次，最多约 1s，
// 覆盖 two-phase 保存中 rename 之后、create 之前的文件缺失窗口。
// 多次失败事件共享同一个重试任务；Add 幂等，成功后补一次加载，
// 因为 Add 本身不会产生事件，two-phase 保存的新内容只能在这里观察到。
func (w *Watcher) scheduleReadd() {
	if !w.readding.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer w.readding.Store(false)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if err := w.watcher.Add(w.path); err == nil {
				// 静默失败：若此刻文件仍在写入（create 后写），下次 Write 事件会重新加载
				if newCfg, err := Load(w.path); err == nil {
					w.cfg.Store(newCfg)
					slog.Info("config reloaded", "path", w.path)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}
