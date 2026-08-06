package config

import (
	"log/slog"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	cfg     atomic.Pointer[Config]
	path    string
	watcher *fsnotify.Watcher
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
			// 编辑器原子写（rename）会丢失 inode watch，重新挂
			if ev.Op&(fsnotify.Rename|fsnotify.Create) != 0 {
				_ = w.watcher.Add(w.path)
			}
			if ev.Op&(fsnotify.Write|fsnotify.Rename|fsnotify.Create) == 0 {
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
