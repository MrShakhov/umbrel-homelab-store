// Disk Live Usage: a tiny read-only poller that streams host disk I/O
// (per disk, per Umbrel app, per process) to a web UI over SSE.
package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

//go:embed web
var webFS embed.FS

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	interval, err := time.ParseDuration(env("INTERVAL", "1s"))
	if err != nil || interval < 250*time.Millisecond {
		log.Fatalf("bad INTERVAL %q (want e.g. 1s, minimum 250ms)", os.Getenv("INTERVAL"))
	}
	topN, err := strconv.Atoi(env("TOP_PROCS", "15"))
	if err != nil || topN < 1 {
		topN = 15
	}

	c := newCollector(
		env("PROC_ROOT", "/proc"),
		env("SYS_ROOT", "/sys"),
		env("CGROUP_ROOT", "/host/cgroup"),
		newDockerClient(env("DOCKER_HOST", "unix:///var/run/docker.sock")),
		topN,
	)
	h := newHub()
	go run(c, h, interval)

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/stream", h.serveSSE)
	mux.HandleFunc("/api/snapshot", h.serveSnapshot)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	addr := env("LISTEN", ":8080")
	log.Printf("disk-live-usage listening on %s, interval %s", addr, interval)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// run samples only while someone is watching, so the app costs nothing when
// the UI is closed. A gap longer than a few intervals resets the baseline.
func run(c *collector, h *hub, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var prev *sample
	for range t.C {
		if !h.active() {
			if prev != nil {
				prev = nil
				h.clear()
			}
			continue
		}
		cur := c.sample()
		if prev != nil && cur.t.Sub(prev.t) < 3*interval {
			b, err := json.Marshal(c.diff(prev, cur))
			if err == nil {
				h.publish(b)
			}
		}
		prev = cur
	}
}
