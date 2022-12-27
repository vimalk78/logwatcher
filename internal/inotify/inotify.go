package inotify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
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

	RootDir = "/var/log/pods"
	SelfDir = "logwatcher"
)

var (
	sizemtx              = sync.Mutex{}
	Sizes                = map[string]int64{}
	HandledEvents uint64 = 0
	SentEvents    uint64 = 0
)

type NotifyEvent struct {
	unix.InotifyEvent
	path string
}

type Notify struct {
	rootDir     string
	namespaces  map[string]struct{}
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
		rootDir:     root,
		fd:          fd,
		inotifyFile: os.NewFile(uintptr(fd), ""),
		mtx:         sync.RWMutex{},
		events:      make(chan NotifyEvent, EventChanSize),
		paths:       map[int]string{},
		watches:     map[string]int{},
		namespaces:  map[string]struct{}{},
	}
	return n, n.WatchDir(root)
}

func (n *Notify) Start() {
	//n.WatchExistingLogs()
	glog.V(0).Infoln("started....")
	glog.Flush()
	MaxEventLoops := 1
	wg := sync.WaitGroup{}
	wg.Add(1)
	go n.ReadLoop(&wg)
	for i := 0; i < MaxEventLoops; i++ {
		wg.Add(1)
		go n.Handleloop(&wg, i)
	}
	wg.Wait()
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
			glog.Errorf("Error in ReadLoop. breaking the loop. err: %v", err)
			// break the loop
			break
		}
		if readbytes <= 0 {
			glog.Errorf("readbytes <= 0. breaking the loop. readbytes: %d", readbytes)
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
			offset += unix.SizeofInotifyEvent + raw.Len
			/*
				if raw.Mask&unix.IN_IGNORED == unix.IN_IGNORED {
					continue
				}
			*/
			e := NotifyEvent{
				InotifyEvent: *raw,
				path:         strings.TrimRight(path, "\x00"),
			}
			n.events <- e
			events += 1
			atomic.AddUint64(&SentEvents, 1)
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
	// start ticker in first goroutine only
	if idx == 0 {
		tickerCh = time.NewTicker(time.Minute * 1).C
	}
	for {
		glog.V(3).Info("--------- going to select ---------")
		select {
		case e := <-n.events:
			handled := false
			atomic.AddUint64(&HandledEvents, 1)
			if e.IsOverFlowErr() {
				glog.Exit("Overflow occured. Exiting program.")
			}
			n.mtx.RLock()
			watchedPath, ok := n.paths[int(e.Wd)]
			n.mtx.RUnlock()
			if !ok {
				glog.Errorf("A watch event received for an unknown watched path. Fd: %d, Event Mask: %x", e.Wd, e.Mask)
				continue
			}
			glog.V(0).Infof("event for watchedPath: %q, for path: %q, mask is: %x", watchedPath, e.path, e.Mask)
			switch {
			case e.IsDir() && e.path != "":
				glog.V(5).Infof("something happened to a subdirectory: %q in %q", e.path, watchedPath)
			case !e.IsDir() && e.path != "":
				glog.V(5).Infof("something happened to a file: %q in %q", e.path, watchedPath)
			case e.IsDir() && e.path == "":
				glog.V(5).Infof("1. something happened to a watchedPath (file or directry) %q", watchedPath)
			case !e.IsDir() && e.path == "":
				glog.V(5).Infof("2. something happened to a watchedPath (file or directry) %q", watchedPath)
			}
			if e.IsDir() {
				switch {
				case e.Mask&unix.IN_CREATE != 0:
					if watchedPath == RootDir {
						glog.V(0).Infof("a new namespace/pod got created %q", e.path)
						n.namespaces[e.path] = struct{}{}
					} else {
						glog.V(0).Infof("a new container got created %q", e.path)
					}
					must(n.WatchDir(filepath.Join(watchedPath, e.path)))
					handled = true

				case e.Mask&unix.IN_DELETE != 0:
					glog.V(0).Infof("a directory got deleted: %q/%q", watchedPath, e.path)
					dir := filepath.Join(watchedPath, e.path)
					fd, ok := n.watches[dir]
					if ok {
						n.RemoveWatch(fd, dir)
					} else {
						glog.Errorf("could not find fd for deleted directory: %q", dir)
					}
					handled = true

				case e.Mask&unix.IN_DELETE_SELF != 0:
					n.RemoveWatch(int(e.Wd), watchedPath)
					glog.V(0).Infof("a watched directory got deleted: %q", e.path)
					handled = true

				case e.Mask&unix.IN_IGNORED != 0:
					glog.V(9).Infof("received IGNORED event for path: %q, ignoring it", filepath.Join(watchedPath, e.path))
					if strings.Contains(watchedPath, "json-log") {
						glog.V(9).Infof("got IGNORED event for path: %q, Mask: %x\n", filepath.Join(watchedPath, e.path), e.Mask)
					}

				default:
					glog.Errorf("unhandled event type. watchedPath: %q, e.path: %q, mask: %x", watchedPath, e.path, e.Mask)
				}
			} else {
				switch {
				case e.Mask&unix.IN_CREATE != 0:
					glog.V(0).Infof("a new log file got created: %q\n", e.path)
					must(n.WatchLogFile(filepath.Join(watchedPath, e.path)))
					file := filepath.Join(watchedPath, e.path)
					err := UpdateFileSize(file)
					if err != nil {
						n.RemoveWatch(int(e.Wd), file)
					}
					handled = true

				case e.Mask&unix.IN_DELETE != 0:
					file := filepath.Join(watchedPath, e.path)
					fd, ok := n.watches[file]
					if ok {
						glog.V(0).Infof("a file got deleted: %q??, fd: %d\n", file, fd)
						n.RemoveWatch(fd, file)
					} else {
						glog.Errorf("could not find watch fd for deleted file %q", file)
					}
					handled = true

				case e.Mask&unix.IN_DELETE_SELF != 0:
					glog.V(0).Infof("a watched file got self deleted: %q??\n", e.path)
					//n.RemoveWatch(int(e.Wd), filepath.Join(watchedPath, e.path))

				case e.Mask&unix.IN_MODIFY != 0:
					glog.V(3).Infof("a file got written: %q\n", watchedPath)
					must(n.WatchLogFile(watchedPath))
					err := UpdateFileSize(watchedPath)
					must(err)
					if err != nil {
						glog.Errorf("Error in stat. file: %q, err: %v", watchedPath, err)
						n.RemoveWatch(int(e.Wd), watchedPath)
					}
					handled = true

				case e.Mask&unix.IN_IGNORED != 0:
					glog.V(9).Infof("got IGNORED event for path: %q\n", filepath.Join(watchedPath, e.path))
					if strings.Contains(watchedPath, "json-log") {
						glog.V(9).Infof("got IGNORED event for path: %q, Mask: %x\n", filepath.Join(watchedPath, e.path), e.Mask)
					}

				case e.Mask&unix.IN_CLOSE_WRITE != 0:
					glog.V(0).Infof("a file opened for writing got closed: %q", watchedPath)
					UpdateFileSize(watchedPath)
					// we add watch because file still exists, and could be written to again
					must(n.WatchLogFile(watchedPath))
					handled = true

				default:
					glog.Errorf("unhandled event type. watchedPath: %q, e.path: %q, mask: %x", watchedPath, e.path, e.Mask)
				}
			}
			if !handled && e.Mask != 0x8000 {
				glog.Errorf("----- event not handled. watchedPath: %q, e.path: %q, mask: %x", watchedPath, e.path, e.Mask)
			}
		case <-tickerCh:
			/**/
			sizemtx.Lock()
			glog.V(0).Infof("sizes(%d): %s\nEventsSent: %d, EventsHandled: %d\n", len(Sizes), func(m map[string]int64) string {
				p, err := json.MarshalIndent(m, "", "  ")
				if err != nil {
					return fmt.Sprintf("%v", err)
				}
				return string(p)
			}(Sizes), SentEvents, HandledEvents)
			sizemtx.Unlock()
			/**/
			/**/
			n.mtx.RLock()
			wl := n.WatchList()
			n.mtx.RUnlock()

			l := strings.Join(wl, ",\n")
			glog.V(0).Infof("Watching paths: \n%s\nTotal watches: %d\n", l, len(wl))
			/**/
		}
	}
	wg.Done()
	glog.V(3).Info("exiting Handleloop")
}

func (n *Notify) WatchExistingLogs() {
	filepath.WalkDir(n.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == n.rootDir {
			return nil
		}
		// do not watch self dir else results in a positive feedback loop
		if d.Name() == SelfDir && d.IsDir() {
			glog.V(0).Infof("skipping reading self dir: %s", SelfDir)
			return filepath.SkipDir
		}
		glog.V(3).Infof("checking dir: %s", path)

		if d.IsDir() {
			glog.V(0).Infof("watching directory %q", path)
			return n.WatchDir(path)
		} else {
			err2 := n.WatchLogFile(path)
			if err2 != nil {
				return err2
			}
			err2 = UpdateFileSize(path)
			//	if err2 != nil {
			//		n.RemoveWatch(0, path)
			//	}
			return err2
		}
		return nil
	})
}

func (n *Notify) WatchList() []string {
	n.mtx.RLock()
	defer n.mtx.RUnlock()

	entries := make([]string, 0, len(n.watches))
	for pathname, fd := range n.watches {
		entries = append(entries, fmt.Sprintf("%6x, %q", fd, pathname))
	}

	return entries
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
	if wfd == -1 {
		glog.V(0).Infof("error in watch for path: %q, err: %v\n", path, err)
		debug.PrintStack()
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
	/*
		entries := make([]string, 0, len(n.watches))
		for pathname := range n.watches {
			entries = append(entries, pathname)
		}
		l := strings.Join(entries, ",\n")
		glog.V(3).Infof("Watching \n%s\n", l)
	*/
	//
	return nil
}

func (n *Notify) RemoveWatch(wfd int, path string) {
	n.mtx.Lock()
	defer n.mtx.Unlock()
	n.removeWatch(wfd, path)
}

func (n *Notify) removeWatch(wfd int, path string) {
	ret, err := unix.InotifyRmWatch(n.fd, uint32(wfd))
	if ret == -1 {
		glog.V(0).Infof("error in watch for path: %q, err: %v\n", path, err)
		debug.PrintStack()
	}
	glog.V(0).Infof("removed watch for path: %q\n", path)
	delete(n.watches, path)
	delete(n.paths, wfd)
	/*
		entries := make([]string, 0, len(n.watches))
		for pathname := range n.watches {
			entries = append(entries, pathname)
		}
		l := strings.Join(entries, ",\n")
		glog.V(3).Infof("Watching \n%s\n", l)
	*/
}
func UpdateFileSize(path string) error {
	s, err := os.Stat(path)
	if err != nil {
		glog.Error("could not stat file: ", path, err)
		return err
	}
	sizemtx.Lock()
	Sizes[path] = s.Size()
	sizemtx.Unlock()
	return nil
}
func must(err error) {
	if err != nil {
		glog.Exit("Exiting with error: ", err)
	}
}
