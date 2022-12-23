package inotify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/golang/glog"
	"golang.org/x/sys/unix"
)

/**
A brief note on inotify flags used for watching files.
======================================================



**/
const (
	FlagsWatchDir = unix.IN_CREATE | // File/directory created in watched directory
		unix.IN_DELETE | // File/directory deleted from watched directory.
		unix.IN_DELETE_SELF //  Watched file/directory was itself deleted.

	FlagsWatchFile = unix.IN_MODIFY | // File was modified (e.g., write(2), truncate(2)).
		unix.IN_ONESHOT | // Monitor the filesystem object corresponding to pathname for one event, then remove from watch list.
		unix.IN_CLOSE_WRITE // File opened for writing was closed.

	EventChanSize = 4096

	// DO NOT USE THIS
	flags uint32 = unix.IN_MOVED_TO | unix.IN_MOVED_FROM |
		unix.IN_CREATE | unix.IN_ATTRIB | unix.IN_MODIFY |
		unix.IN_MOVE_SELF | unix.IN_DELETE | unix.IN_DELETE_SELF
)

var (
	Sizes              = map[string]int64{}
	TotalEvents uint64 = 0
)

type NotifyEvent struct {
	unix.InotifyEvent
	path string
}

type Notify struct {
	fd          int
	inotifyFile *os.File // used for read()ing events
	watches     map[string]int
	paths       map[int]string
	mtx         sync.RWMutex
	events      chan NotifyEvent
}

func (ne NotifyEvent) IsOverFlowErr() bool {
	return ne.Mask&unix.IN_Q_OVERFLOW == unix.IN_Q_OVERFLOW
}

func (ne NotifyEvent) IsDir() bool {
	return ne.Mask&unix.IN_ISDIR == unix.IN_ISDIR // Subject of this event is a directory.
}

func New(root string) (*Notify, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New("input must be a directory")
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	n := &Notify{
		fd:          fd,
		inotifyFile: os.NewFile(uintptr(fd), ""),
		mtx:         sync.RWMutex{},
		events:      make(chan NotifyEvent, EventChanSize),
		paths:       map[int]string{},
		watches:     map[string]int{},
	}
	return n, n.WatchDir(root)
}

func (n *Notify) WatchDir(dir string) error {
	n.mtx.Lock()
	defer n.mtx.Unlock()
	return n.watchPathWith(dir, FlagsWatchDir)
}

func (n *Notify) WatchLogFile(path string) error {
	n.mtx.Lock()
	defer n.mtx.Unlock()
	return n.watchPathWith(path, FlagsWatchFile)
}

func (n *Notify) watchPathWith(path string, flags uint32) error {
	// n.mtx is already held
	path = filepath.Clean(path)
	wfd, err := unix.InotifyAddWatch(n.fd, path, flags)
	if err != nil {
		glog.V(3).Infof("error in watch for path: %q, err: %v\n", path, err)
		return err
	}
	glog.V(3).Infof("added watch for path: %q\n", path)
	/*
		oldwfd, ok := n.watches[path]
		if ok {
			//debug.PrintStack()
			// should not happen in ideal case
			//n.removeWatch(oldwfd, path)
		}
	*/
	n.watches[path] = wfd
	n.paths[wfd] = path
	//
	entries := make([]string, 0, len(n.watches))
	for pathname := range n.watches {
		entries = append(entries, pathname)
	}
	l := strings.Join(entries, ",\n")
	glog.V(3).Infof("Watching \n%s\n", l)
	//
	return nil
}

func (n *Notify) RemoveWatch(wfd int, path string) {
	n.mtx.Lock()
	defer n.mtx.Unlock()
	n.removeWatch(wfd, path)
}

func (n *Notify) removeWatch(wfd int, path string) {
	unix.InotifyRmWatch(n.fd, uint32(wfd))
	glog.V(3).Infof("removed watch for path: %q\n", path)
	delete(n.watches, path)
	delete(n.paths, wfd)
	entries := make([]string, 0, len(n.watches))
	for pathname := range n.watches {
		entries = append(entries, pathname)
	}
	l := strings.Join(entries, ",\n")
	glog.V(3).Infof("Watching \n%s\n", l)
}

func (n *Notify) ReadLoop(wg *sync.WaitGroup) {
	var (
		buf [unix.SizeofInotifyEvent * EventChanSize]byte // Buffer for a maximum of 4096 raw events
	)
	defer func() {
		close(n.events)
		n.mtx.Lock()
		for fd := range n.paths {
			unix.InotifyRmWatch(n.fd, uint32(fd))
		}
		n.mtx.Unlock()
		n.inotifyFile.Close()
	}()

	for {
		readbytes, err := n.inotifyFile.Read(buf[:])
		if err != nil {
			if errors.Unwrap(err) == io.EOF {
			}
			// break the loop
			break
		}
		if readbytes <= 0 {
			break
		}
		events := 0
		consumed := 0
		var offset uint32 = 0
		for offset <= uint32(readbytes-unix.SizeofInotifyEvent) {
			raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			consumed += unix.SizeofInotifyEvent
			path := string(buf[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+raw.Len])
			consumed += int(raw.Len)
			e := NotifyEvent{
				InotifyEvent: *raw,
				path:         strings.TrimRight(path, "\x00"),
			}
			n.events <- e
			events += 1
			offset += unix.SizeofInotifyEvent + e.Len
		}
		if readbytes-consumed != 0 {
			glog.V(0).Infof("Read %d bytes, %d events, consumed %d bytes, remaining %d bytes", readbytes, events, consumed, (readbytes - consumed))
		}
	}
	wg.Done()
	glog.V(0).Infoln("exiting ReadLoop")
}

func (n *Notify) Handleloop(wg *sync.WaitGroup, idx int) {
	var tickerCh <-chan time.Time
	if idx == 0 {
		tickerCh = time.NewTicker(time.Minute * 1).C
	}
	for {
		glog.V(3).Info("--------- going to select ---------")
		select {
		case e := <-n.events:
			atomic.AddUint64(&TotalEvents, 1)
			if e.IsOverFlowErr() {
				glog.Exit("Overflow occured. Exiting program.")
			}
			n.mtx.RLock()
			watchedPath, ok := n.paths[int(e.Wd)]
			n.mtx.RUnlock()
			if !ok {
				glog.Error("A watch event received for an unknown watched path. Event Mask:", e.Mask)
				continue
			}
			glog.V(3).Infof("event for watchedPath: %q, for path: %q, mask is: %x", watchedPath, e.path, e.Mask)
			if e.IsDir() {
				switch {
				case e.Mask&unix.IN_CREATE != 0:
					glog.V(3).Infof("a new directory got created: %q", e.path)
					must(n.WatchDir(filepath.Join(watchedPath, e.path)))

				case e.Mask&unix.IN_DELETE != 0:
					glog.V(3).Infof("a directory got deleted: %q", e.path)
					n.RemoveWatch(int(e.Wd), filepath.Join(watchedPath, e.path))

				case e.Mask&unix.IN_DELETE_SELF != 0:
					n.RemoveWatch(int(e.Wd), watchedPath)
					glog.V(3).Infof("a watched directory got deleted: %q", e.path)

				case e.Mask&unix.IN_IGNORED != 0:
					glog.V(5).Infof("got IGNORED event for path: %q", filepath.Join(watchedPath, e.path))

				default:
					glog.Error("unhandled event type. path: %q, mask: %x", e.path, e.Mask)
				}
			} else {
				switch {
				case e.Mask&unix.IN_CREATE != 0:
					glog.V(3).Infof("a new file got created: %q\n", e.path)
					must(n.WatchLogFile(filepath.Join(watchedPath, e.path)))
					file := filepath.Join(watchedPath, e.path)
					if s, err := os.Stat(file); err == nil {
						glog.V(3).Infof("file: %q size: %d\n", file, s.Size())
						Sizes[file] = s.Size()
					} else if errors.Is(err, os.ErrNotExist) {
						n.RemoveWatch(int(e.Wd), file)
					} else {
						glog.Error(err)
					}

				case e.Mask&unix.IN_DELETE != 0:
					glog.V(3).Infof("a file got deleted: %q??, fd: %d\n", e.path, e.Wd)
					n.RemoveWatch(int(e.Wd), filepath.Join(watchedPath, e.path))

				case e.Mask&unix.IN_DELETE_SELF != 0:
					glog.V(3).Infof("a watched file got deleted: %q??\n", e.path)
					n.RemoveWatch(int(e.Wd), filepath.Join(watchedPath, e.path))

				case e.Mask&unix.IN_MODIFY != 0:
					glog.V(3).Infof("a file got written: %q\n", watchedPath)
					must(n.WatchLogFile(watchedPath))
					if s, err := os.Stat(watchedPath); err == nil {
						glog.V(3).Infof("file: %q size: %d\n", watchedPath, s.Size())
						Sizes[watchedPath] = s.Size()
					} else if errors.Is(err, os.ErrNotExist) {
						n.RemoveWatch(int(e.Wd), watchedPath)
					} else {
						glog.Error(err)
					}

				case e.Mask&unix.IN_IGNORED != 0:
					glog.V(3).Infof("got IGNORED event for path: %q\n", filepath.Join(watchedPath, e.path))

				case e.Mask&unix.IN_CLOSE_WRITE != 0:
					glog.V(0).Infof("a file opened for writing got closed: %q", watchedPath)
					if s, err := os.Stat(watchedPath); err == nil {
						glog.V(3).Infof("file: %q size: %d\n", watchedPath, s.Size())
						Sizes[watchedPath] = s.Size()
					} else if errors.Is(err, os.ErrNotExist) {
						n.RemoveWatch(int(e.Wd), watchedPath)
					} else {
						glog.Error(err)
					}

				default:
					glog.V(0).Infof("unhandled event type. watchedPath: %q, e.path: %q, mask: %x", watchedPath, e.path, e.Mask)
				}
			}
		case <-tickerCh:
			n.mtx.RLock()
			wl := n.WatchList()
			n.mtx.RUnlock()

			l := strings.Join(wl, ",\n")
			glog.V(0).Infof("Watching (%d) paths: \n%s\n", len(wl), l)
			/**/
			glog.V(0).Infof("sizes(%d): %s\nEventsReceived: %d\n", len(Sizes), func(m map[string]int64) string {
				p, err := json.MarshalIndent(m, "", "  ")
				if err != nil {
					return fmt.Sprintf("%v", err)
				}
				return string(p)
			}(Sizes), TotalEvents)
			/**/
		}
	}
	wg.Done()
	glog.V(3).Info("exiting Handleloop")
}

func (n *Notify) WatchList() []string {
	n.mtx.RLock()
	defer n.mtx.RUnlock()

	entries := make([]string, 0, len(n.watches))
	for pathname := range n.watches {
		entries = append(entries, pathname)
	}

	return entries
}

func must(err error) {
	if err != nil {
		glog.Exit("Exiting with error: ", err)
	}
}
