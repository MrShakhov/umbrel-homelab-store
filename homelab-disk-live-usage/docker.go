package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// dockerClient does exactly one read-only call: GET /containers/json.
// DOCKER_HOST may be unix:///path or tcp://host:port (e.g. a socket proxy).
type dockerClient struct {
	hc   *http.Client
	base string
}

func newDockerClient(host string) *dockerClient {
	if path, ok := strings.CutPrefix(host, "unix://"); ok {
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}}
		return &dockerClient{hc: &http.Client{Transport: tr, Timeout: 2 * time.Second}, base: "http://docker"}
	}
	return &dockerClient{hc: &http.Client{Timeout: 2 * time.Second}, base: "http://" + strings.TrimPrefix(host, "tcp://")}
}

type container struct {
	id, name, app, service string
	cgDir                  string // relative to CGROUP_ROOT, "" if not found
}

// Umbrel names containers <app-id>_<service>_1.
var umbrelName = regexp.MustCompile(`^([a-z0-9-]+)_(.+)_\d+$`)

func (d *dockerClient) containers() ([]*container, error) {
	resp, err := d.hc.Get(d.base + "/containers/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker API: %s", resp.Status)
	}
	var raw []struct {
		ID     string `json:"Id"`
		Names  []string
		Labels map[string]string
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]*container, 0, len(raw))
	for _, r := range raw {
		ct := &container{id: r.ID}
		if len(r.Names) > 0 {
			ct.name = strings.TrimPrefix(r.Names[0], "/")
		}
		if m := umbrelName.FindStringSubmatch(ct.name); m != nil {
			ct.app, ct.service = m[1], m[2]
		} else if p := r.Labels["com.docker.compose.project"]; p != "" {
			ct.app, ct.service = p, r.Labels["com.docker.compose.service"]
		} else {
			ct.app, ct.service = ct.name, ""
		}
		out = append(out, ct)
	}
	return out, nil
}
