// Binary entrypoint: build the store from the deterministic trial
// identities, listen on an ephemeral loopback port, print READY <port>,
// and serve HTTP until killed. The Node launcher (main.ts) reads the
// port, registers it with the trial environment, and shuts this process
// down when the trial ends.
//
// Environment (set by the trial harness):
//   MAKEFUN_SHOP_TOKEN the bearer token clients must present (empty = dev mode)
//   MAKEFUN_TRIAL_NOW  the deterministic "now" in epoch seconds

package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	trialNow := int64(1735689600)
	if raw := os.Getenv("MAKEFUN_TRIAL_NOW"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			trialNow = n
		}
	}
	viewer, _ := deriveIdentities()
	initViewerJSON(viewer)
	store := newStore(viewer, token, trialNow)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("READY %d\n", port)
	serve(ln, func(w *Responder, req *Request) {
		dispatch(w, req, store)
	})
}
