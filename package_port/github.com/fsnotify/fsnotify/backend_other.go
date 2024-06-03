//go:build !zos

// Tags altered by Wharf (added !zos)
//
package fsnotify

import (
	"fmt"
	"runtime"
)

type Watcher struct{}

func NewWatcher() (*Watcher, error) {
	return nil, fmt.Errorf("fsnotify not supported on %s", runtime.GOOS)
}

func (w *Watcher) Close() error {
	return nil
}

func (w *Watcher) Add(name string) error {
	return nil
}

func (w *Watcher) Remove(name string) error {
	return nil
}
