package auth

import (
	"sync"
	"sync/atomic"
)

// The auth generation counts in-process changes that can narrow what an
// existing credential may do (§7.5). Key caches clear themselves when it
// moves, and open SSE/WebSocket connections re-check their principal.
var (
	generation atomic.Uint64
	genMu      sync.Mutex            // orders Bump against Watch
	genCh      = make(chan struct{}) // closed and replaced by every Bump
)

// Generation returns the current auth generation.
func Generation() uint64 {
	return generation.Load()
}

// Bump advances the auth generation, wakes every channel returned by Watch,
// and returns the new generation. Call it after a key PATCH or DELETE, an
// anonymous-policy PUT and a system merge. Changes made by the CLI happen in
// another process and cannot bump it.
func Bump() uint64 {
	genMu.Lock()
	defer genMu.Unlock()
	g := generation.Add(1)
	close(genCh)
	genCh = make(chan struct{})
	return g
}

// Watch returns the current generation together with a channel that the next
// Bump closes, so a long-lived connection can re-check its principal at once
// rather than at its next periodic check. Call Watch again after each wake-up.
func Watch() (uint64, <-chan struct{}) {
	genMu.Lock()
	defer genMu.Unlock()
	return generation.Load(), genCh
}
