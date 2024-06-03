//go:build zos

// Tags altered by Wharf (added zos)
//
package fsnotify

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

type Watcher struct {
	Events chan Event

	Errors chan error

	fd          int
	mu          sync.Mutex
	inotifyFile *os.File
	watches     map[string]*watch
	paths       map[int]string
	done        chan struct{}
	doneResp    chan struct{}
}

func NewWatcher() (*Watcher, error) {

	fd, errno := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if fd == -1 {
		return nil, errno
	}

	w := &Watcher{
		fd:          fd,
		inotifyFile: os.NewFile(uintptr(fd), ""),
		watches:     make(map[string]*watch),
		paths:       make(map[int]string),
		Events:      make(chan Event),
		Errors:      make(chan error),
		done:        make(chan struct{}),
		doneResp:    make(chan struct{}),
	}

	go w.readEvents()
	return w, nil
}

func (w *Watcher) sendEvent(e Event) bool {
	select {
	case w.Events <- e:
		return true
	case <-w.done:
	}
	return false
}

func (w *Watcher) sendError(err error) bool {
	select {
	case w.Errors <- err:
		return true
	case <-w.done:
		return false
	}
}

func (w *Watcher) isClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.isClosed() {
		w.mu.Unlock()
		return nil
	}

	close(w.done)
	w.mu.Unlock()

	err := w.inotifyFile.Close()
	if err != nil {
		return err
	}

	<-w.doneResp

	return nil
}

func (w *Watcher) Add(name string) error {
	name = filepath.Clean(name)
	if w.isClosed() {
		return errors.New("inotify instance already closed")
	}

	var flags uint32 = unix.IN_MOVED_TO | unix.IN_MOVED_FROM |
		unix.IN_CREATE | unix.IN_ATTRIB | unix.IN_MODIFY |
		unix.IN_MOVE_SELF | unix.IN_DELETE | unix.IN_DELETE_SELF

	w.mu.Lock()
	defer w.mu.Unlock()
	watchEntry := w.watches[name]
	if watchEntry != nil {
		flags |= watchEntry.flags | unix.IN_MASK_ADD
	}
	wd, errno := unix.InotifyAddWatch(w.fd, name, flags)
	if wd == -1 {
		return errno
	}

	if watchEntry == nil {
		w.watches[name] = &watch{wd: uint32(wd), flags: flags}
		w.paths[wd] = name
	} else {
		watchEntry.wd = uint32(wd)
		watchEntry.flags = flags
	}

	return nil
}

func (w *Watcher) Remove(name string) error {
	name = filepath.Clean(name)

	w.mu.Lock()
	defer w.mu.Unlock()
	watch, ok := w.watches[name]

	if !ok {
		return fmt.Errorf("%w: %s", ErrNonExistentWatch, name)
	}

	delete(w.paths, int(watch.wd))
	delete(w.watches, name)

	success, errno := unix.InotifyRmWatch(w.fd, watch.wd)
	if success == -1 {

		return errno
	}

	return nil
}

func (w *Watcher) WatchList() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	entries := make([]string, 0, len(w.watches))
	for pathname := range w.watches {
		entries = append(entries, pathname)
	}

	return entries
}

type watch struct {
	wd    uint32
	flags uint32
}

func (w *Watcher) readEvents() {
	defer func() {
		close(w.doneResp)
		close(w.Errors)
		close(w.Events)
	}()

	var (
		buf   [unix.SizeofInotifyEvent * 4096]byte
		errno error
	)
	for {

		if w.isClosed() {
			return
		}

		n, err := w.inotifyFile.Read(buf[:])
		switch {
		case errors.Unwrap(err) == os.ErrClosed:
			return
		case err != nil:
			if !w.sendError(err) {
				return
			}
			continue
		}

		if n < unix.SizeofInotifyEvent {
			var err error
			if n == 0 {

				err = io.EOF
			} else if n < 0 {

				err = errno
			} else {

				err = errors.New("notify: short read in readEvents()")
			}
			if !w.sendError(err) {
				return
			}
			continue
		}

		var offset uint32

		for offset <= uint32(n-unix.SizeofInotifyEvent) {
			var (
				raw     = (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
				mask    = uint32(raw.Mask)
				nameLen = uint32(raw.Len)
			)

			if mask&unix.IN_Q_OVERFLOW != 0 {
				if !w.sendError(ErrEventOverflow) {
					return
				}
			}

			w.mu.Lock()
			name, ok := w.paths[int(raw.Wd)]

			if ok && mask&unix.IN_DELETE_SELF == unix.IN_DELETE_SELF {
				delete(w.paths, int(raw.Wd))
				delete(w.watches, name)
			}
			w.mu.Unlock()

			if nameLen > 0 {

				bytes := (*[unix.PathMax]byte)(unsafe.Pointer(&buf[offset+unix.SizeofInotifyEvent]))[:nameLen:nameLen]

				name += "/" + strings.TrimRight(string(bytes[0:nameLen]), "\000")
			}

			event := w.newEvent(name, mask)

			if mask&unix.IN_IGNORED == 0 {
				if !w.sendEvent(event) {
					return
				}
			}

			offset += unix.SizeofInotifyEvent + nameLen
		}
	}
}

func (w *Watcher) newEvent(name string, mask uint32) Event {
	e := Event{Name: name}
	if mask&unix.IN_CREATE == unix.IN_CREATE || mask&unix.IN_MOVED_TO == unix.IN_MOVED_TO {
		e.Op |= Create
	}
	if mask&unix.IN_DELETE_SELF == unix.IN_DELETE_SELF || mask&unix.IN_DELETE == unix.IN_DELETE {
		e.Op |= Remove
	}
	if mask&unix.IN_MODIFY == unix.IN_MODIFY {
		e.Op |= Write
	}
	if mask&unix.IN_MOVE_SELF == unix.IN_MOVE_SELF || mask&unix.IN_MOVED_FROM == unix.IN_MOVED_FROM {
		e.Op |= Rename
	}
	if mask&unix.IN_ATTRIB == unix.IN_ATTRIB {
		e.Op |= Chmod
	}
	return e
}
