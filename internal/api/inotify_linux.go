//go:build linux

package api

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// inotifyWatcher watches a directory tree recursively with inotify and
// coalesces everything into a single "something changed" signal.
type inotifyWatcher struct {
	fd   int
	ch   chan struct{}
	stop chan struct{}
	once sync.Once

	mu     sync.Mutex
	wdPath map[int]string
	pathWD map[string]int
}

const inotifyMask = unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_CLOSE_WRITE |
	unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_DELETE_SELF |
	unix.IN_MOVE_SELF

// newFSWatcher installs recursive inotify watches over dirs. It returns nil
// when inotify is unavailable or none of the directories could be watched,
// so the caller falls back to periodic polling.
func newFSWatcher(dirs []string) fsWatcher {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil
	}
	w := &inotifyWatcher{
		fd:     fd,
		ch:     make(chan struct{}, 1),
		stop:   make(chan struct{}),
		wdPath: map[int]string{},
		pathWD: map[string]int{},
	}
	watched := false
	for _, d := range dirs {
		if w.addTree(d) {
			watched = true
		}
	}
	if !watched {
		unix.Close(fd)
		return nil
	}
	go w.read()
	return w
}

func (w *inotifyWatcher) Events() <-chan struct{} { return w.ch }

// Close signals the reader to stop; it closes the fd itself so there is no
// window where a recycled descriptor is polled.
func (w *inotifyWatcher) Close() {
	w.once.Do(func() { close(w.stop) })
}

func (w *inotifyWatcher) add(dir string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pathWD[dir]; ok {
		return true
	}
	wd, err := unix.InotifyAddWatch(w.fd, dir, inotifyMask)
	if err != nil {
		return false
	}
	w.wdPath[wd] = dir
	w.pathWD[dir] = wd
	return true
}

func (w *inotifyWatcher) addTree(root string) bool {
	added := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir // skip hidden dirs, like ingest
		}
		if w.add(path) {
			added = true
		}
		return nil
	})
	return added
}

func (w *inotifyWatcher) read() {
	defer unix.Close(w.fd)
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		// poll with a timeout so Close is noticed promptly
		fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 1000)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if n == 0 {
			continue
		}
		for {
			nr, err := unix.Read(w.fd, buf)
			if err != nil {
				if err == unix.EAGAIN {
					break
				}
				if err == unix.EINTR {
					continue
				}
				return
			}
			if nr <= 0 {
				break
			}
			w.parse(buf[:nr])
			select {
			case w.ch <- struct{}{}:
			default:
			}
		}
	}
}

func (w *inotifyWatcher) parse(b []byte) {
	const evSize = unix.SizeofInotifyEvent
	for off := 0; off+evSize <= len(b); {
		raw := (*unix.InotifyEvent)(unsafe.Pointer(&b[off]))
		wd := int(raw.Wd)
		mask := uint32(raw.Mask)
		nameLen := int(raw.Len)
		name := ""
		if nameLen > 0 {
			start, end := off+evSize, off+evSize+nameLen
			if end > len(b) {
				break
			}
			name = strings.TrimRight(string(b[start:end]), "\x00")
		}
		if mask&unix.IN_IGNORED != 0 {
			w.mu.Lock()
			if p, ok := w.wdPath[wd]; ok {
				delete(w.wdPath, wd)
				delete(w.pathWD, p)
			}
			w.mu.Unlock()
		}
		// a newly created/moved-in directory gets its own recursive watch
		if mask&unix.IN_ISDIR != 0 && mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 &&
			name != "" && !strings.HasPrefix(name, ".") {
			w.mu.Lock()
			base := w.wdPath[wd]
			w.mu.Unlock()
			if base != "" {
				w.addTree(filepath.Join(base, name))
			}
		}
		off += evSize + nameLen
	}
}
