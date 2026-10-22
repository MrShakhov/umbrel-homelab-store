package main

import (
	"net/http"
	"sync"
	"time"
)

// hub fans the latest snapshot out to SSE subscribers.
type hub struct {
	mu       sync.Mutex
	subs     map[chan []byte]struct{}
	last     []byte
	lastPoll time.Time // last /api/snapshot request, keeps sampling alive for pollers
}

func newHub() *hub { return &hub{subs: map[chan []byte]struct{}{}} }

func (h *hub) active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0 || time.Since(h.lastPoll) < 30*time.Second
}

func (h *hub) publish(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = b
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // slow client: drop this frame
		}
	}
}

func (h *hub) clear() {
	h.mu.Lock()
	h.last = nil
	h.mu.Unlock()
}

func (h *hub) serveSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 4)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	last := h.last
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("X-Accel-Buffering", "no")
	w.Write([]byte("retry: 3000\n\n"))
	if last != nil {
		writeEvent(w, last)
	}
	fl.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			if writeEvent(w, b) != nil {
				return
			}
			fl.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, b []byte) error {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	w.Write(b)
	_, err := w.Write([]byte("\n\n"))
	return err
}

func (h *hub) serveSnapshot(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	h.lastPoll = time.Now()
	last := h.last
	h.mu.Unlock()
	if last == nil {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "warming up, retry in a couple of seconds", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(last)
}
