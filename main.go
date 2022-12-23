package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ViaQ/logwatcher/internal/inotify"
	"github.com/golang/glog"
)

var usage = `
`

var watchDir = flag.String("watch_dir", func() string {
	wd, _ := os.Getwd()
	return wd
}(), "directory to watch for logs")

func exitWithMessage(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, filepath.Base(os.Args[0])+": "+format+"\n", a...)
	fmt.Print("\n" + usage)
	os.Exit(1)
}

func help() {
	fmt.Printf("%s [command] [arguments]\n\n", filepath.Base(os.Args[0]))
	fmt.Print(usage)
	os.Exit(0)
}

func main() {
	flag.Parse()
	wd, err := filepath.Abs(*watchDir)
	if err != nil {
		glog.Exit("error in arguments.", os.Args)
	}
	glog.V(0).Infof("Watching directory %s", wd)
	n, err := inotify.New(wd)
	if err != nil {
		glog.Exit("error in starting watcher: ", err)
		os.Exit(-1)
	}
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
	// block forever
	//<-make(chan struct{})

	glog.Info("Bye..")
}
