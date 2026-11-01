package main

// denma: on SIGTERM or SIGINT, campaigns stop where they are and save their progress.

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/knadh/listmonk/internal/manager"
)

// denmaStopWait is how long the managers have to save.
const denmaStopWait = 8 * time.Second

// denmaAwaitShutdown handles SIGTERM and SIGINT for the process.
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
		manager.DenmaStopDeliveries() // no more automation e-mails handed over (cmd/denma_auto_series.go)
		var wg sync.WaitGroup
		for _, a := range apps {
			if a == nil || a.manager == nil {
				continue
			}
			if a.crons != nil {
				a.crons.Stop() // no more automation runs
			}
			wg.Add(1)
			go func(a *App) {
				defer wg.Done()
				if !a.manager.DenmaStop(denmaStopWait) {
					a.log.Printf("denma: a campaign didn't save its progress in %v; it resumes from its last checkpoint", denmaStopWait)
				}
			}(a)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Automation e-mails being sent finish (cmd/denma_automations.go).
			if !manager.DenmaDeliveriesWait(denmaStopWait) {
				lo.Printf("denma: automation e-mails being sent didn't finish in %v; they're marked as maybe delivered", denmaStopWait)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !denmaDrainBounces(app, denmaStopWait) { // cmd/denma_bounces.go
				lo.Printf("denma: bounces read from the mailbox weren't all kept in %v", denmaStopWait)
			}
		}()
		wg.Wait()
		denmaDaily.Save()       // cmd/denma_daily.go
		denmaCenterSends.Save() // and each center's
		lo.Printf("denma: campaigns saved; exiting")
		os.Exit(0)
	}()
}
