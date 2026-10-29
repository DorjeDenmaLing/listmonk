package main

// denma: what the import's preview (cmd/denma_importpreview.go) keeps and
// limits, so that a large file doesn't take the server's memory:
//
//   - one preview at a time in the process (denmaPreviewTurn, as image
//     uploads are: cmd/denma_media.go);
//   - the file is read from the upload as a stream, never whole into memory;
//   - the page uploads a file once: it's kept in a temp file for a few
//     minutes (one per person and center), and the page then sends only a
//     token and the settings. The import, or a new file, ends it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	// denmaKeptFor is how long a kept file is kept after its last preview.
	denmaKeptFor = 10 * time.Minute
	// denmaKeptMax is how many files are kept at once, for all centers.
	denmaKeptMax = 20
	// denmaPreviewParamsMax is the most a preview's settings can be.
	denmaPreviewParamsMax = 1 << 20
)

// denmaPreviewTurn is the turn to make a preview: one at a time.
var denmaPreviewTurn = make(chan struct{}, 1)

// denmaPreviewWait waits for the turn, and gives the way to end it, or the
// context's error if it ends first.
func denmaPreviewWait(ctx context.Context) (func(), error) {
	select {
	case denmaPreviewTurn <- struct{}{}:
		return func() { <-denmaPreviewTurn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// denmaKeptFile is an uploaded file kept for its preview.
type denmaKeptFile struct {
	token, name, path string
	size              int64
	timer             *time.Timer
	at                time.Time
}

// denmaKeptFiles are the kept files, by center and person.
type denmaKeptFiles struct {
	mu    sync.Mutex
	files map[string]*denmaKeptFile
	dir   string
	ttl   time.Duration
}

var denmaKept = &denmaKeptFiles{files: map[string]*denmaKeptFile{}, ttl: denmaKeptFor}

func denmaKeptKey(center string, user int) string {
	return fmt.Sprintf("%s/%d", center, user)
}

// dirOK makes the folder for the files, and clears the old ones in it the
// first time.
func (k *denmaKeptFiles) dirOK() error {
	if k.dir != "" {
		return nil
	}
	dir := filepath.Join(os.TempDir(), "denma-import-preview")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	old, _ := os.ReadDir(dir)
	for _, e := range old {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > k.ttl {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	k.dir = dir
	return nil
}

// put keeps the file read from src for key, in place of its last, and gives
// its token.
func (k *denmaKeptFiles) put(key, name string, src io.Reader) (string, error) {
	k.mu.Lock()
	err := k.dirOK()
	dir := k.dir
	k.mu.Unlock()
	if err != nil {
		return "", err
	}
	out, err := os.CreateTemp(dir, "f")
	if err != nil {
		return "", err
	}
	size, err := io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out.Name())
		return "", err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	f := &denmaKeptFile{token: hex.EncodeToString(b), name: name, path: out.Name(), size: size, at: time.Now()}

	k.mu.Lock()
	defer k.mu.Unlock()
	k.dropLocked(key)
	for len(k.files) >= denmaKeptMax {
		oldest := ""
		for key, o := range k.files {
			if oldest == "" || o.at.Before(k.files[oldest].at) {
				oldest = key
			}
		}
		k.dropLocked(oldest)
	}
	f.timer = time.AfterFunc(k.ttl, func() { k.expire(key, f) })
	k.files[key] = f
	return f.token, nil
}

// open opens the file kept for key with the token (and keeps it longer), or
// is false if there's none.
func (k *denmaKeptFiles) open(key, token string) (*os.File, *denmaKeptFile, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f := k.files[key]
	if f == nil || f.token != token {
		return nil, nil, false
	}
	fh, err := os.Open(f.path)
	if err != nil {
		k.dropLocked(key)
		return nil, nil, false
	}
	f.at = time.Now()
	f.timer.Reset(k.ttl)
	return fh, f, true
}

// drop forgets the file kept for key.
func (k *denmaKeptFiles) drop(key string) {
	k.mu.Lock()
	k.dropLocked(key)
	k.mu.Unlock()
}

func (k *denmaKeptFiles) dropLocked(key string) {
	if f := k.files[key]; f != nil {
		f.timer.Stop()
		os.Remove(f.path)
		delete(k.files, key)
	}
}

// expire forgets f if it's still the one kept, and not used since.
func (k *denmaKeptFiles) expire(key string, f *denmaKeptFile) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.files[key] == f && time.Since(f.at) >= k.ttl {
		k.dropLocked(key)
	}
}

// denmaPreviewForm reads a preview's request, as multipart/form-data, without
// holding the file in memory: its settings (params), and the file, which is
// kept and given a token, or the token of one kept already. The file is
// opened, and its name and token are given.
func (a *App) denmaPreviewForm(c echo.Context, key string) (params string, f *os.File, name, token string, err error) {
	mr, err := c.Request().MultipartReader()
	if err != nil {
		return "", nil, "", "", echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("import.invalidFile", "error", err.Error()))
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, "", "", echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("import.invalidFile", "error", err.Error()))
		}
		switch part.FormName() {
		case "params":
			b, err := io.ReadAll(io.LimitReader(part, denmaPreviewParamsMax))
			if err != nil {
				return "", nil, "", "", echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("import.invalidParams", "error", err.Error()))
			}
			params = string(b)
		case "token":
			b, _ := io.ReadAll(io.LimitReader(part, 100))
			token = string(b)
		case "file":
			t, err := denmaKept.put(key, part.FileName(), part)
			if err != nil {
				return "", nil, "", "", echo.NewHTTPError(http.StatusBadRequest, a.i18n.Ts("import.invalidFile", "error", err.Error()))
			}
			token = t
		}
		part.Close()
	}
	fh, kept, ok := denmaKept.open(key, token)
	if !ok {
		return "", nil, "", "", echo.NewHTTPError(http.StatusGone, "The file isn't kept any longer: pick it again.")
	}
	return params, fh, kept.name, token, nil
}
