package main

// denma: the automations' schedule (views/denma-automations.html, under the
// list): what they've run and what's coming up, by day.
//
// Everything is counted in the database, by day (in the viewer's time zone)
// and automation, so hundreds of automations and thousands of people waiting
// stay quick: the page shows a chart of the days, the automations of the day
// chosen, and the people of one of them only when asked, a page at a time.
// Who ran is denma_automation_sends (the last run for each person and
// automation); who's coming up is what denmaAutoPendingSQL finds, due when
// their wait is over (now, if that's passed and they're still waiting: the
// next run, or a sender's domain SES hasn't verified, or the daily limit).
// Whether they meet its conditions is checked when it runs. People's
// addresses are only for users who can see all subscribers.

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

const (
	denmaSchedMaxDays = 62
	denmaSchedPerPage = 50
)

// denmaAutoDueSQL is what's coming up (cond on a, the automation), with when
// it's due: when the wait is over, or now if that's passed.
func denmaAutoDueSQL(cond string) string {
	return `SELECT p.automation_id, p.subscriber_id, GREATEST(p.first_at + make_interval(mins => p.delay_minutes), NOW()) AS due
		FROM (` + denmaAutoPendingSQL(cond) + `) p`
}

// schedQuery is a schedule request: the viewer's time zone, and an
// automation (0 for all).
type schedQuery struct {
	loc    *time.Location
	tz     string
	autoID int
}

func (a *App) schedQuery(c echo.Context) (schedQuery, error) {
	if !denmaAutomationsOn(a) {
		return schedQuery{}, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	q := schedQuery{tz: c.QueryParam("tz")}
	loc, err := time.LoadLocation(q.tz)
	if q.tz == "" || err != nil {
		loc, q.tz = time.UTC, "UTC"
	}
	q.loc = loc
	q.autoID, _ = strconv.Atoi(c.QueryParam("automation"))
	return q, nil
}

// day parses a YYYY-MM-DD day param into the start of that day there.
func (q schedQuery) day(s string) (time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", s, q.loc)
	if err != nil {
		return time.Time{}, echo.NewHTTPError(http.StatusBadRequest, "Give the day as YYYY-MM-DD.")
	}
	return d, nil
}

// cond is the automation condition for denmaAutoPendingSQL, with the
// automation as parameter n.
func (q schedQuery) cond(n int) string {
	if q.autoID == 0 {
		return "TRUE"
	}
	return fmt.Sprintf("a.id = $%d", n)
}

// denmaSchedDay is a day in the chart.
type denmaSchedDay struct {
	Day      string `db:"day" json:"day"`
	Ran      int    `db:"ran" json:"ran"` // sent or done
	Failed   int    `db:"failed" json:"failed"`
	Skipped  int    `db:"skipped" json:"skipped"`
	Upcoming int    `db:"upcoming" json:"upcoming"`
}

// DenmaAutoSchedule returns the days from `from` (YYYY-MM-DD, by default two
// weeks ago) for `days` days (28), with what ran and what's coming up on
// each, and how many are coming up after them.
func (a *App) DenmaAutoSchedule(c echo.Context) error {
	q, err := a.schedQuery(c)
	if err != nil {
		return err
	}
	now := time.Now().In(q.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, q.loc)
	start := today.AddDate(0, 0, -14)
	if s := c.QueryParam("from"); s != "" {
		if start, err = q.day(s); err != nil {
			return err
		}
	}
	n, _ := strconv.Atoi(c.QueryParam("days"))
	if n <= 0 || n > denmaSchedMaxDays {
		n = 28
	}
	end := start.AddDate(0, 0, n)

	args := []any{q.tz, start, end}
	if q.autoID > 0 {
		args = append(args, q.autoID)
	}
	autoCond := "TRUE"
	if q.autoID > 0 {
		autoCond = "d.automation_id = $4"
	}
	var days []denmaSchedDay
	if err := a.db.Select(&days, `
		WITH ran AS (
			SELECT (d.sent_at AT TIME ZONE $1)::DATE AS day,
				COUNT(*) FILTER (WHERE d.status IN ('sent', 'done')) AS ran,
				COUNT(*) FILTER (WHERE d.status = 'failed') AS failed,
				COUNT(*) FILTER (WHERE d.status = 'skipped') AS skipped
			FROM denma_automation_sends d
			WHERE d.sent_at >= $2 AND d.sent_at < $3 AND `+autoCond+`
			GROUP BY 1
		), up AS (
			SELECT (due AT TIME ZONE $1)::DATE AS day, COUNT(*) AS upcoming
			FROM (`+denmaAutoDueSQL(q.cond(4))+`) u
			WHERE due >= $2 AND due < $3
			GROUP BY 1
		)
		SELECT TO_CHAR(g.day, 'YYYY-MM-DD') AS day, COALESCE(ran.ran, 0) AS ran, COALESCE(ran.failed, 0) AS failed,
			COALESCE(ran.skipped, 0) AS skipped, COALESCE(up.upcoming, 0) AS upcoming
		FROM generate_series(($2 AT TIME ZONE $1)::DATE, ($3 AT TIME ZONE $1)::DATE - 1, '1 day') g(day)
		LEFT JOIN ran ON ran.day = g.day LEFT JOIN up ON up.day = g.day
		ORDER BY g.day`, args...); err != nil {
		a.log.Printf("denma: error getting the automations' schedule: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error getting the schedule.")
	}

	var later int
	laterArgs := []any{end}
	if q.autoID > 0 {
		laterArgs = append(laterArgs, q.autoID)
	}
	if err := a.db.Get(&later, `SELECT COUNT(*) FROM (`+denmaAutoDueSQL(q.cond(2))+`) u WHERE due >= $1`, laterArgs...); err != nil {
		a.log.Printf("denma: error getting the automations' schedule: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error getting the schedule.")
	}
	return c.JSON(http.StatusOK, okResp{map[string]any{
		"days": days, "later": later, "today": today.Format("2006-01-02"),
		"from": start.Format("2006-01-02"), "to": end.AddDate(0, 0, -1).Format("2006-01-02"),
	}})
}

// denmaSchedGroup is an automation on a day: who it ran for, and who's
// coming up.
type denmaSchedGroup struct {
	ID       int       `db:"id" json:"id"`
	Name     string    `db:"name" json:"name"`
	Ran      int       `db:"ran" json:"ran"`
	Sent     int       `db:"sent" json:"sent"`
	Failed   int       `db:"failed" json:"failed"`
	Skipped  int       `db:"skipped" json:"skipped"`
	Upcoming int       `db:"upcoming" json:"upcoming"`
	NextAt   null.Time `db:"next_at" json:"next_at"` // the first coming up
	Summary  string    `db:"-" json:"summary"`
}

// DenmaAutoScheduleDay returns the automations that ran or are coming up on
// a day.
func (a *App) DenmaAutoScheduleDay(c echo.Context) error {
	q, err := a.schedQuery(c)
	if err != nil {
		return err
	}
	start, err := q.day(c.QueryParam("day"))
	if err != nil {
		return err
	}
	end := start.AddDate(0, 0, 1)
	args := []any{start, end}
	autoCond := "TRUE"
	if q.autoID > 0 {
		args = append(args, q.autoID)
		autoCond = "d.automation_id = $3"
	}
	var groups []denmaSchedGroup
	if err := a.db.Select(&groups, `
		WITH ran AS (
			SELECT d.automation_id AS id,
				COUNT(*) FILTER (WHERE d.status IN ('sent', 'done')) AS ran,
				COUNT(*) FILTER (WHERE d.status = 'sent') AS sent,
				COUNT(*) FILTER (WHERE d.status = 'failed') AS failed,
				COUNT(*) FILTER (WHERE d.status = 'skipped') AS skipped
			FROM denma_automation_sends d
			WHERE d.sent_at >= $1 AND d.sent_at < $2 AND `+autoCond+`
			GROUP BY 1
		), up AS (
			SELECT automation_id AS id, COUNT(*) AS upcoming, MIN(due) AS next_at
			FROM (`+denmaAutoDueSQL(q.cond(3))+`) u
			WHERE due >= $1 AND due < $2
			GROUP BY 1
		)
		SELECT a.id, a.name, COALESCE(ran.ran, 0) AS ran, COALESCE(ran.sent, 0) AS sent, COALESCE(ran.failed, 0) AS failed,
			COALESCE(ran.skipped, 0) AS skipped, COALESCE(up.upcoming, 0) AS upcoming, up.next_at
		FROM denma_automations a
		LEFT JOIN ran ON ran.id = a.id LEFT JOIN up ON up.id = a.id
		WHERE ran.id IS NOT NULL OR up.id IS NOT NULL
		ORDER BY up.next_at NULLS LAST, a.name`, args...); err != nil {
		a.log.Printf("denma: error getting the automations' day: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error getting the schedule.")
	}

	// What each does, in words.
	if len(groups) > 0 {
		ids := make([]int, len(groups))
		for i, g := range groups {
			ids[i] = g.ID
		}
		var autos []denmaAutomation
		if err := a.db.Select(&autos, denmaAutoSelectSQL+` WHERE a.id = ANY($1)`, pq.Array(ids)); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		for i := range groups {
			for _, x := range autos {
				if x.ID == groups[i].ID {
					groups[i].Summary = x.Summary()
				}
			}
		}
	}
	if groups == nil {
		groups = []denmaSchedGroup{}
	}
	return c.JSON(http.StatusOK, okResp{groups})
}

// denmaSchedPerson is someone an automation ran for, or will.
type denmaSchedPerson struct {
	ID     int       `db:"id" json:"id"`
	Email  string    `db:"email" json:"email"`
	Name   string    `db:"name" json:"name"`
	At     time.Time `db:"at" json:"at"`
	Status string    `db:"status" json:"status"` // upcoming, sent, done, failed, skipped
	Did    string    `db:"did" json:"did"`
	Error  string    `db:"error" json:"error"`
	Total  int       `db:"total" json:"-"`
}

// DenmaAutoSchedulePeople returns who an automation ran for on a day, or
// who's coming up (kind=upcoming), a page at a time.
func (a *App) DenmaAutoSchedulePeople(c echo.Context) error {
	q, err := a.schedQuery(c)
	if err != nil {
		return err
	}
	u := auth.GetUser(c)
	if !denmaAllLists(&u) || !u.HasPerm("subscribers:get_all") {
		return echo.NewHTTPError(http.StatusForbidden, "Seeing who needs access to all subscribers.")
	}
	if q.autoID == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "Choose an automation.")
	}
	start, err := q.day(c.QueryParam("day"))
	if err != nil {
		return err
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	if page < 1 {
		page = 1
	}
	args := []any{start, start.AddDate(0, 0, 1), q.autoID, denmaSchedPerPage, (page - 1) * denmaSchedPerPage}
	var out []denmaSchedPerson
	if c.QueryParam("kind") == "upcoming" {
		err = a.db.Select(&out, `SELECT s.id, s.email, s.name, u.due AS at, 'upcoming' AS status, '' AS did, '' AS error, COUNT(*) OVER () AS total
			FROM (`+denmaAutoDueSQL("a.id = $3")+`) u JOIN subscribers s ON s.id = u.subscriber_id
			WHERE u.due >= $1 AND u.due < $2
			ORDER BY u.due, s.id LIMIT $4 OFFSET $5`, args...)
	} else {
		err = a.db.Select(&out, `SELECT s.id, s.email, s.name, d.sent_at AS at, d.status, d.did, d.error, COUNT(*) OVER () AS total
			FROM denma_automation_sends d JOIN subscribers s ON s.id = d.subscriber_id
			WHERE d.automation_id = $3 AND d.sent_at >= $1 AND d.sent_at < $2
			ORDER BY d.sent_at DESC, s.id LIMIT $4 OFFSET $5`, args...)
	}
	if err != nil {
		a.log.Printf("denma: error getting an automation's people: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error getting the schedule.")
	}
	total := 0
	if len(out) > 0 {
		total = out[0].Total
	}
	if out == nil {
		out = []denmaSchedPerson{}
	}
	return c.JSON(http.StatusOK, okResp{map[string]any{"results": out, "total": total, "page": page, "per_page": denmaSchedPerPage}})
}
