package main

// denma: when e-mail that waits for a center's sending hours will go
// (cmd/denma_sendwindow.go): the next opening. The Calendar, the
// automations' schedule and the confirmation before a campaign is scheduled
// show it.

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

const denmaMaxHoursAt = 200

// next is the first time at or after t within the hours: t itself if they're
// open then or off.
func (w *denmaSendWindow) next(t time.Time) time.Time {
	if w.open(t) {
		return t
	}
	l := t.In(w.loc)
	m := l.Hour()*60 + l.Minute()
	for i := 0; i <= 7; i++ {
		d := time.Date(l.Year(), l.Month(), l.Day()+i, w.from/60, w.from%60, 0, 0, w.loc)
		if !slices.Contains(w.Days, int(d.Weekday())) || (i == 0 && m >= w.from) {
			continue
		}
		if d.After(t) {
			return d
		}
	}
	return t
}

// describe says the hours in words: "Mon-Fri 08:00-21:00, America/Halifax".
func (w *denmaSendWindow) describe() string {
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	days := slices.Clone(w.Days)
	slices.Sort(days)
	var parts []string
	for i := 0; i < len(days); {
		j := i
		for j+1 < len(days) && days[j+1] == days[j]+1 {
			j++
		}
		switch {
		case j-i >= 2:
			parts = append(parts, names[days[i]]+"-"+names[days[j]])
		default:
			for k := i; k <= j; k++ {
				parts = append(parts, names[days[k]])
			}
		}
		i = j + 1
	}
	return fmt.Sprintf("%s %s-%s, %s", strings.Join(parts, ", "), w.From, w.To, w.loc)
}

// goesSQL is SQL for when e-mail due at e (a timestamptz expression) will go:
// e, or the next opening if it's outside the hours. It matches next.
func (w *denmaSendWindow) goesSQL(e string) string {
	if w == nil || !w.On {
		return e
	}
	days := make([]string, len(w.Days))
	for i, d := range w.Days {
		days[i] = fmt.Sprint(d)
	}
	tz := pq.QuoteLiteral(w.loc.String())
	from := fmt.Sprintf("'%02d:%02d'::TIME", w.from/60, w.from%60)
	to := fmt.Sprintf("'%02d:%02d'::TIME", w.to/60, w.to%60)
	l := "(" + e + " AT TIME ZONE " + tz + ")"
	ds := "ARRAY[" + strings.Join(days, ",") + "]"
	return fmt.Sprintf(`(CASE WHEN EXTRACT(DOW FROM %[1]s)::INT = ANY(%[2]s) AND %[1]s::TIME >= %[3]s AND %[1]s::TIME < %[4]s THEN %[5]s
		ELSE COALESCE((SELECT (%[1]s::DATE + g + %[3]s) AT TIME ZONE %[6]s FROM generate_series(0, 7) g
			WHERE EXTRACT(DOW FROM %[1]s::DATE + g)::INT = ANY(%[2]s) AND (g > 0 OR %[1]s::TIME < %[3]s) ORDER BY g LIMIT 1), %[5]s) END)`,
		l, ds, from, to, e, tz)
}

// denmaHours is the center's sending hours when they're on, otherwise nil.
func (a *App) denmaHours() *denmaSendWindow {
	if !a.ko.Bool("denma.multi_center") || a.ko.String("denma.center") == "" {
		return nil
	}
	if w := denmaWindowOf(a.ko.String("denma.center")).Load(); w != nil && w.On && w.loc != nil {
		return w
	}
	return nil
}

// denmaHoursNote is what the admin shows of the sending hours.
type denmaHoursNote struct {
	On   bool       `json:"on"`
	Text string     `json:"text"`
	Goes *time.Time `json:"goes"` // the next opening, if t is outside them
}

// denmaHoursAt is the note for e-mail going at t.
func (a *App) denmaHoursAt(t time.Time) denmaHoursNote {
	w := a.denmaHours()
	if w == nil {
		return denmaHoursNote{}
	}
	n := denmaHoursNote{On: true, Text: w.describe()}
	if g := w.next(t); !g.Equal(t) {
		n.Goes = &g
	}
	return n
}

// DenmaSendHours answers, for each time given (at, RFC3339), when e-mail
// going then will actually go: null if it's within the center's sending
// hours or they're off.
func (a *App) DenmaSendHours(c echo.Context) error {
	ats := c.QueryParams()["at"]
	if len(ats) > denmaMaxHoursAt {
		return echo.NewHTTPError(http.StatusBadRequest, "Too many times.")
	}
	w := a.denmaHours()
	goes := make([]*time.Time, len(ats))
	for i, s := range ats {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Give the times as RFC 3339.")
		}
		if w != nil {
			if g := w.next(t); !g.Equal(t) {
				goes[i] = &g
			}
		}
	}
	n := a.denmaHoursAt(time.Now())
	return c.JSON(http.StatusOK, okResp{map[string]any{"on": n.On, "text": n.Text, "goes": goes}})
}
