package main

// denma: a center's sending hours (Config -> General): campaigns and
// automation e-mails go only on its days, between its hours, in its time
// zone, so that a campaign held back by the daily limit doesn't trickle out
// at 3 a.m. Outside them, a campaign's messages wait (manager
// DenmaSetWindowWait, before the daily limit's wait, so that no room is
// claimed overnight) and go on when they open; automations that send e-mail
// wait to run. Confirmations, password resets, notices, tests and the API's
// transactional e-mails always go. Off by default. A save applies at once, as
// the daily cap does: a campaign waiting for the hours would hold up the
// center's reload.

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // every time zone, whatever the system has

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/manager"
)

// denmaSendWindow is a center's sending hours (denma.send_window), in its
// time zone (denma.timezone).
type denmaSendWindow struct {
	On   bool   `json:"on"`
	Days []int  `json:"days"` // 0 Sunday … 6 Saturday
	From string `json:"from"` // HH:MM
	To   string `json:"to"`   // HH:MM, after From

	loc      *time.Location
	from, to int // minutes into the day
}

// denmaSendWindowDefault is a center's until it sets its own: off.
var denmaSendWindowDefault = denmaSendWindow{Days: []int{0, 1, 2, 3, 4, 5, 6}, From: "08:00", To: "21:00"}

// denmaHHMM is minutes into the day from HH:MM.
func denmaHHMM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("give the hours as HH:MM, such as 08:00")
	}
	return t.Hour()*60 + t.Minute(), nil
}

// check validates w, for time zone tz, and makes it ready to use.
func (w *denmaSendWindow) check(tz string) error {
	if w.Days == nil {
		w.Days = []int{}
	}
	loc, err := time.LoadLocation(tz)
	if tz == "" || err != nil {
		if w.On || tz != "" {
			return fmt.Errorf("Choose the center's time zone.")
		}
		loc = time.UTC
	}
	w.loc = loc
	if !w.On {
		return nil
	}
	if w.from, err = denmaHHMM(w.From); err != nil {
		return fmt.Errorf("Sending hours: %v.", err)
	}
	if w.to, err = denmaHHMM(w.To); err != nil {
		return fmt.Errorf("Sending hours: %v.", err)
	}
	if w.to <= w.from {
		return fmt.Errorf("Sending hours: the end has to be after the start, on the same day.")
	}
	if len(w.Days) == 0 {
		return fmt.Errorf("Sending hours: choose at least one day.")
	}
	for _, d := range w.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("Sending hours: unknown day.")
		}
	}
	return nil
}

// open reports whether t is within the hours (always, if they're off).
func (w *denmaSendWindow) open(t time.Time) bool {
	if w == nil || !w.On {
		return true
	}
	t = t.In(w.loc)
	m := t.Hour()*60 + t.Minute()
	return slices.Contains(w.Days, int(t.Weekday())) && m >= w.from && m < w.to
}

// denmaWindows holds each center's hours (by slug), changed at once on a
// save.
var denmaWindows sync.Map // slug -> *atomic.Pointer[denmaSendWindow]

func denmaWindowOf(slug string) *atomic.Pointer[denmaSendWindow] {
	p, _ := denmaWindows.LoadOrStore(slug, &atomic.Pointer[denmaSendWindow]{})
	return p.(*atomic.Pointer[denmaSendWindow])
}

// denmaWindowFrom reads a center's hours from its settings.
func denmaWindowFrom(ko *koanf.Koanf) *denmaSendWindow {
	w := denmaSendWindowDefault
	if ko.Exists("denma.send_window") {
		_ = ko.Unmarshal("denma.send_window", &w)
	}
	if err := w.check(ko.String("denma.timezone")); err != nil {
		w.On = false // saved before it was checked; off rather than wrong
	}
	return &w
}

// denmaCenterWindow sets a center's hours from its settings (ko) and makes
// its campaign messages wait for them. Called with denmaCenterCap.
func denmaCenterWindow(mgr *manager.Manager, ko *koanf.Koanf) {
	if !ko.Bool("denma.multi_center") || ko.String("denma.center") == "" {
		return
	}
	p := denmaWindowOf(ko.String("denma.center"))
	p.Store(denmaWindowFrom(ko))
	mgr.DenmaSetWindowWait(func(stopped func() bool) bool {
		for !p.Load().open(time.Now()) {
			if stopped() {
				return false
			}
			time.Sleep(time.Second)
		}
		return true
	})
}

// denmaSendingOpen reports whether a's center may send campaigns and
// automation e-mails now.
func (a *App) denmaSendingOpen() bool {
	if !a.ko.Bool("denma.multi_center") || a.ko.String("denma.center") == "" {
		return true
	}
	return denmaWindowOf(a.ko.String("denma.center")).Load().open(time.Now())
}
