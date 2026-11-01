package main

// denma: each center's own log lines, with its address after the file (denmaLog).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"strings"
	"sync"

	"github.com/knadh/koanf/v2"
)

// denmaRedactLog writes the process's log without e-mail addresses, each as its reference (denmaAddrRef).
type denmaRedactLog struct{ w io.Writer }

func (r denmaRedactLog) Write(p []byte) (int, error) {
	if bytes.IndexByte(p, '@') < 0 {
		return r.w.Write(p)
	}
	out := reDenmaAddress.ReplaceAllFunc(p, func(m []byte) []byte { return []byte(denmaAddrRef(string(m))) })
	if _, err := r.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// denmaAddrRef stands for an address in a log: the start of its hash, as denma_email_hash has it.
func denmaAddrRef(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "(address " + hex.EncodeToString(h[:])[:10] + ")"
}

var (
	denmaCenterLogs sync.Map // slug -> *log.Logger
	denmaQuietLog   = log.New(io.Discard, "", 0)
)

// denmaLog is the logger for an app's config: the process's for the hub, a tagging one for a center.
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

// denmaSetupLog is for setup lines the same in every center: the hub's logger, or none.
func denmaSetupLog(ko *koanf.Koanf) *log.Logger {
	if ko.String("denma.center") != "" {
		return denmaQuietLog
	}
	return lo
}
