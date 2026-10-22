package main

// denma: Go's profiler (net/http/pprof), for measuring memory and
// goroutines, served only when DENMA_PPROF names an address to listen on
// (dev sets 127.0.0.1:6060, inside its container). Off otherwise.

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
