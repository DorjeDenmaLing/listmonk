package main

// denma: a clean shutdown. listmonk only handles SIGHUP (a reload), so a
// deploy's SIGTERM (docker stop) ended the process mid-send, like a crash:
// each running campaign resumed from its last saved checkpoint, sending
// those after it a second copy. On SIGTERM or SIGINT, every campaign
// manager (the hub's and each center's) stops its running campaigns where
// they are and saves their progress (manager.DenmaStop), then the process
// exits. The campaigns stay running, and resume on the next start.

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// denmaStopWait is how long the managers have to save, within Docker's
// default 10 seconds before it kills the process.
const denmaStopWait = 8 * time.Second

// denmaAwaitShutdown handles SIGTERM and SIGINT for the process. Called by
// main.
func denmaAwaitShutdown(app *App) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-ch
		apps := []*App{app}
		if denmaHub != nil {
			for _, ctr := range denmaHub.loaded() {
				apps = append(apps, ctr.app)
			}
		}
		lo.Printf("denma: %v: stopping the running campaigns where they are", sig)
		var wg sync.WaitGroup
		for _, a := range apps {
			if a == nil || a.manager == nil {
				continue
			}
			wg.Add(1)
			go func(a *App) {
				defer wg.Done()
				if !a.manager.DenmaStop(denmaStopWait) {
					a.log.Printf("denma: a campaign didn't save its progress in %v; it resumes from its last checkpoint", denmaStopWait)
				}
			}(a)
		}
		wg.Wait()
		denmaDaily.Save() // cmd/denma_daily.go
		lo.Printf("denma: campaigns saved; exiting")
		os.Exit(0)
	}()
}
