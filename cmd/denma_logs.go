package main

// denma: each center's own log lines. All apps write to the process's log
// (stdout and the buffer the hub's Logs page shows), so a center's lines
// carry its address after the file, as in "init.go:820: boulder: ...":
// denmaLog gives an app its logger by its config, and listmonk's setup
// functions take it in place of the process's (lo, shadowed at their top,
// marked lines in cmd/init.go and cmd/main.go), as do the parts they set up
// (campaign manager, core, notifier, cron jobs) and the app (a.log). Only the
// hub shows the log (Settings -> Logs), with every center's lines; it can
// show one center's (views/logs.html).
//
// The three setup lines whose values are the hub's in every center (the
// database, media store and SMTP servers) are logged by the hub only
// (denmaSetupLog): 300 centers made a start fill the buffer with them.

import (
	"io"
	"log"
	"sync"

	"github.com/knadh/koanf/v2"
)

var (
	denmaCenterLogs sync.Map // slug -> *log.Logger
	denmaQuietLog   = log.New(io.Discard, "", 0)
)

// denmaLog is the logger for an app's config: the process's for the hub (or
// a single install), one that tags its lines for a center.
func denmaLog(ko *koanf.Koanf) *log.Logger {
	slug := ko.String("denma.center")
	if slug == "" {
		return lo
	}
	if l, ok := denmaCenterLogs.Load(slug); ok {
		return l.(*log.Logger)
	}
	l, _ := denmaCenterLogs.LoadOrStore(slug, log.New(lo.Writer(), slug+": ", lo.Flags()|log.Lmsgprefix))
	return l.(*log.Logger)
}

// denmaSetupLog is for setup lines whose values are the same in every center:
// the hub's logger, or none in a center.
func denmaSetupLog(ko *koanf.Koanf) *log.Logger {
	if ko.String("denma.center") != "" {
		return denmaQuietLog
	}
	return lo
}
