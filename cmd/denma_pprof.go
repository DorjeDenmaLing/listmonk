package main

// denma: Go's profiler (net/http/pprof), served only when DENMA_PPROF names an address.

import (
	"net/http"
	_ "net/http/pprof" // its handlers, on http.DefaultServeMux
	"os"
)

func init() {
	if addr := os.Getenv("DENMA_PPROF"); addr != "" {
		go func() {
			lo.Printf("denma: profiler on %s: %v", addr, http.ListenAndServe(addr, nil))
		}()
	}
}
