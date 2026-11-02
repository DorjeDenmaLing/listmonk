package main

// denma: the hub says when the fork's ddl branch has commits this build lacks, and a superadmin can ask for them (listmonk-infra/listmonk-update.sh looks them up and does the update).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	denmaUpdateCache   = time.Hour
	denmaUpdateRefresh = time.Minute     // the soonest "Check now" asks again
	denmaCheckWait     = 2 * time.Minute // a check not taken by then isn't being answered
	denmaUpdateStuck   = 2 * time.Hour   // an update still under way after this was stopped
)

var denmaFullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

type denmaUpdateCommit struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"`
	Date    time.Time `json:"date"`
}

// denmaUpdateCheck is how the branch compares with a build, from the updater's available.json.
type denmaUpdateCheck struct {
	Running string              `json:"running"`
	Head    string              `json:"head"`
	Status  string              `json:"status"` // ahead, identical, behind or diverged
	Ahead   int                 `json:"ahead"`
	Commits []denmaUpdateCommit `json:"commits"`
	Checked time.Time           `json:"checked"`
	Error   string              `json:"error,omitempty"`
}

// denmaUpdateState is how an update asked for is going, written by the server's updater.
type denmaUpdateState struct {
	State   string `json:"state"` // checking, building, installing, done or failed
	Commit  string `json:"commit"`
	Message string `json:"message"`
	At      string `json:"at"`
}

// denmaRunningCommit is this build's commit, if its build string names one.
func (a *App) denmaRunningCommit() string {
	if m := denmaBuildCommit.FindStringSubmatch(a.about.Build); m != nil {
		return m[1]
	}
	return ""
}

// denmaReadUpdateCheck reads the updater's last comparison.
func denmaReadUpdateCheck(dir string) denmaUpdateCheck {
	var c denmaUpdateCheck
	f, err := os.Open(filepath.Join(dir, "available.json"))
	if err != nil {
		return c
	}
	defer f.Close()
	_ = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&c)
	return c
}

// denmaWriteRequest puts a file for the updater, whole.
func denmaWriteRequest(dir, name, value string) error {
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// denmaFindUpdate gives the comparison for the running commit, asking the updater for a new one if it's older than maxAge; checking is true while one is asked for.
func denmaFindUpdate(dir, running string, maxAge time.Duration, now time.Time) (found denmaUpdateCheck, checking bool) {
	found = denmaReadUpdateCheck(dir)
	if found.Running != running {
		found = denmaUpdateCheck{Running: running}
	}
	if st, err := os.Stat(filepath.Join(dir, "check")); err == nil {
		if now.Sub(st.ModTime()) < denmaCheckWait || denmaUpdateBusy(denmaReadUpdateState(dir)) {
			return found, true
		}
		_ = os.Remove(filepath.Join(dir, "check"))
		found.Error = "The server's updater isn't answering."
		return found, false
	}
	if !found.Checked.IsZero() && now.Sub(found.Checked) < maxAge {
		return found, false
	}
	if err := denmaWriteRequest(dir, "check", running); err != nil {
		found.Error = "The server's updater can't be asked."
		return found, false
	}
	return found, true
}

// denmaUpdateDir is where the app and the server's updater talk; it's there only where updates are set up.
func denmaUpdateDir() string {
	if d := os.Getenv("DENMA_UPDATE_DIR"); d != "" {
		return d
	}
	return "/var/lib/listmonk-update"
}

func denmaReadUpdateState(dir string) denmaUpdateState {
	var s denmaUpdateState
	f, err := os.Open(filepath.Join(dir, "status.json"))
	if err != nil {
		return s
	}
	defer f.Close()
	_ = json.NewDecoder(io.LimitReader(f, 8<<10)).Decode(&s)
	if at, err := time.Parse(time.RFC3339, s.At); denmaUpdateBusy(s) && (err != nil || time.Since(at) > denmaUpdateStuck) {
		s.State, s.Message = "failed", "The update stopped without finishing. See the /listmonk/build and /listmonk/deploy logs."
	}
	return s
}

func denmaUpdateBusy(s denmaUpdateState) bool {
	return s.State == "checking" || s.State == "building" || s.State == "installing"
}

// denmaRequestUpdate asks the updater for a commit, once.
func denmaRequestUpdate(dir, commit string) error {
	if _, err := os.Stat(filepath.Join(dir, "request")); err == nil || denmaUpdateBusy(denmaReadUpdateState(dir)) {
		return errors.New("An update is already under way.")
	}
	return denmaWriteRequest(dir, "request", commit)
}

func (a *App) denmaInHub() bool {
	return denmaHub == nil || a.urlCfg.RootPath == denmaHub.current().urlCfg.RootPath
}

// DenmaUpdateInfo answers whether there are new commits and how an update is going.
func (a *App) DenmaUpdateInfo(c echo.Context) error {
	if !a.denmaInHub() {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	maxAge := denmaUpdateCache
	if c.QueryParam("refresh") != "" {
		maxAge = denmaUpdateRefresh
	}
	dir := denmaUpdateDir()
	running := a.denmaRunningCommit()
	_, err := os.Stat(dir)
	setUp := err == nil
	found, checking := denmaUpdateCheck{Running: running}, false
	switch {
	case running == "":
		found.Error = "This build doesn't say which commit it is."
	case !setUp:
		found.Error = "Updates aren't set up on this server."
	default:
		found, checking = denmaFindUpdate(dir, running, maxAge, time.Now())
	}
	return c.JSON(http.StatusOK, okResp{struct {
		denmaUpdateCheck
		Checking bool             `json:"checking"`
		SetUp    bool             `json:"set_up"`
		Update   denmaUpdateState `json:"update"`
	}{found, checking, setUp, denmaReadUpdateState(dir)}})
}

// DenmaStartUpdate asks for the branch's newest commit: {"commit": ...}, the one the person was shown.
func (a *App) DenmaStartUpdate(c echo.Context) error {
	if !a.denmaInHub() {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	var req struct {
		Commit string `json:"commit"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	dir := denmaUpdateDir()
	if _, err := os.Stat(dir); err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "Updates aren't set up on this server.")
	}
	found := denmaReadUpdateCheck(dir)
	switch {
	case found.Running == "" || found.Running != a.denmaRunningCommit() || found.Error != "":
		return echo.NewHTTPError(http.StatusBadRequest, "Check for updates again first.")
	case found.Status != "ahead":
		return echo.NewHTTPError(http.StatusBadRequest, "There's nothing newer to update to.")
	case !denmaFullSHA.MatchString(req.Commit) || req.Commit != found.Head:
		return echo.NewHTTPError(http.StatusBadRequest, "There are newer commits than the ones shown. Check them, then update.")
	}
	if err := denmaRequestUpdate(dir, req.Commit); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return c.JSON(http.StatusOK, okResp{true})
}
