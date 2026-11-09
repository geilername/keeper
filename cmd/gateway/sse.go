package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Broadcaster fans photo snapshots out to every connected SSE client.
//
// Publish never blocks: each client has a small buffer, and a client that
// falls behind is dropped (its channel is closed). The browser's EventSource
// reconnects on its own and reloads the list, so a slow tab costs itself a
// refresh instead of stalling the consumer.
type Broadcaster struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	closed  bool
}

const clientBuffer = 64

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{clients: map[chan []byte]struct{}{}}
}

// Subscribe registers a client. ok is false once the broadcaster is closed.
func (b *Broadcaster) Subscribe() (ch chan []byte, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, false
	}
	ch = make(chan []byte, clientBuffer)
	b.clients[ch] = struct{}{}
	return ch, true
}

// Unsubscribe removes a client. Safe to call after the broadcaster dropped
// or closed it.
func (b *Broadcaster) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.clients[ch]; ok {
		delete(b.clients, ch)
		close(ch)
	}
}

// Publish sends v (JSON-encoded once) to every client.
func (b *Broadcaster) Publish(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("sse: encode", "err", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- data:
		default: // slow client: drop it, it will reconnect
			delete(b.clients, ch)
			close(ch)
		}
	}
}

// Close disconnects every client and refuses new ones. http.Server.Shutdown
// waits for open connections, and an SSE stream never goes idle by itself,
// so Close runs from RegisterOnShutdown.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for ch := range b.clients {
		delete(b.clients, ch)
		close(ch)
	}
}

// Clients is the number of connected clients (tests, logs).
func (b *Broadcaster) Clients() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

const (
	heartbeat    = 15 * time.Second
	writeTimeout = 10 * time.Second
)

// ServeSSE streams "photo" events until the client goes away, the client is
// dropped, or the server shuts down. Every exit path unsubscribes, so no
// goroutine or channel outlives the request.
func (b *Broadcaster) ServeSSE(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	ch, ok := b.Subscribe()
	if !ok {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer b.Unsubscribe(ch)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// retry: how long EventSource waits before reconnecting.
	if _, err := fmt.Fprint(w, "retry: 2000\n\n"); err != nil || rc.Flush() != nil {
		return
	}

	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		var frame string
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-ch:
			if !ok {
				return // dropped as slow, or server shutting down
			}
			frame = "event: photo\ndata: " + string(data) + "\n\n"
		case <-tick.C:
			frame = ": heartbeat\n\n"
		}
		// A peer that vanished without closing the connection must not
		// block a write forever.
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := fmt.Fprint(w, frame); err != nil || rc.Flush() != nil {
			return
		}
	}
}
