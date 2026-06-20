// Package stack manages the tmmscope receiver: a standalone Prometheus
// (remote_write receiver) + Grafana (TMM Real-Time dashboard) docker compose
// project, its auto-negotiated host ports, and the discovery file producers
// read to learn where to push.
package stack

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/mwiget/tmmscope/internal/assets"
)

//go:embed compose.tmpl.yaml
var composeTemplate string

const (
	projectName   = "tmmscope"
	promContainer = "tmmscope-prometheus"
	grafContainer = "tmmscope-grafana"
	remoteWritePath = "/api/v1/write"
)

// Endpoints is the discovery contract producers (tmmlitectl, ocibnkctl) read,
// either from `tmmscope endpoint --json` or from endpoints.json in the project
// directory. The host in the URLs is always localhost — a hint for local use;
// cluster producers substitute their own host gateway IP and keep the PORT.
type Endpoints struct {
	Running    bool             `json:"running"`
	Prometheus PrometheusTarget `json:"prometheus"`
	Grafana    GrafanaTarget    `json:"grafana"`
	UpdatedAt  string           `json:"updated_at"`
}

type PrometheusTarget struct {
	Port            int    `json:"port"`
	URL             string `json:"url"`
	RemoteWriteURL  string `json:"remote_write_url"`
	RemoteWritePath string `json:"remote_write_path"`
}

type GrafanaTarget struct {
	Port         int    `json:"port"`
	URL          string `json:"url"`
	DashboardURL string `json:"dashboard_url"`
}

// UpOptions configures `up`. Zero ports mean "negotiate from the default".
type UpOptions struct {
	PrometheusPort       int
	GrafanaPort          int
	GrafanaAdminPassword string
}

// ProjectDir returns the on-disk project directory ($XDG_CONFIG_HOME/tmmscope
// or ~/.config/tmmscope), creating nothing.
func ProjectDir() (string, error) {
	if x := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); x != "" {
		return filepath.Join(x, "tmmscope"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "tmmscope"), nil
}

func endpointsPath() (string, error) {
	dir, err := ProjectDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "endpoints.json"), nil
}

// LoadEndpoints reads the last-written discovery file. It does NOT re-check
// liveness — callers wanting truth should use Status.
func LoadEndpoints() (*Endpoints, error) {
	p, err := endpointsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var e Endpoints
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Status returns the recorded endpoints with a freshly checked Running flag.
func Status() (*Endpoints, error) {
	e, err := LoadEndpoints()
	if err != nil {
		if os.IsNotExist(err) {
			return &Endpoints{Running: false}, nil
		}
		return nil, err
	}
	e.Running = containersRunning()
	return e, nil
}

// Up renders the stack, brings it up (waiting for health), and writes the
// discovery file. It is idempotent: a re-run reuses the previously chosen ports
// when the stack is already ours.
func Up(opts UpOptions) (*Endpoints, error) {
	dir, err := ProjectDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := assets.WriteTelemetry(dir); err != nil {
		return nil, fmt.Errorf("writing telemetry assets: %w", err)
	}

	// Idempotency: if our containers are already running, reuse the ports they
	// actually publish (the source of truth) so a re-run never moves them. Fall
	// back to the last-recorded ports from the discovery file.
	ours := containersRunning()
	var claimedProm, claimedGraf int
	if ours {
		claimedProm = publishedPort(promContainer, "9090/tcp")
		claimedGraf = publishedPort(grafContainer, "3000/tcp")
	}
	if claimedProm == 0 || claimedGraf == 0 {
		if prev, err := LoadEndpoints(); err == nil {
			if claimedProm == 0 {
				claimedProm = prev.Prometheus.Port
			}
			if claimedGraf == 0 {
				claimedGraf = prev.Grafana.Port
			}
		}
	}

	// Preferred port precedence: an explicit flag wins; otherwise a running
	// stack's current port keeps it stable; otherwise the well-known default.
	wantProm := preferredPort(opts.PrometheusPort, claimedProm, DefaultPrometheusPort)
	wantGraf := preferredPort(opts.GrafanaPort, claimedGraf, DefaultGrafanaPort)

	promPort, err := pickPort(wantProm, DefaultPrometheusPort, claimedProm, ours)
	if err != nil {
		return nil, fmt.Errorf("selecting Prometheus port: %w", err)
	}
	grafPort, err := pickPort(wantGraf, DefaultGrafanaPort, claimedGraf, ours)
	if err != nil {
		return nil, fmt.Errorf("selecting Grafana port: %w", err)
	}

	pw := opts.GrafanaAdminPassword
	if pw == "" {
		pw = "tmmscope"
	}

	if err := renderCompose(dir, promPort, grafPort, pw); err != nil {
		return nil, err
	}

	if err := compose(dir, "up", "-d", "--wait", "--wait-timeout", "120", "--remove-orphans"); err != nil {
		return nil, fmt.Errorf("docker compose up: %w", err)
	}

	e := buildEndpoints(promPort, grafPort, true)
	if err := writeEndpoints(dir, e); err != nil {
		return nil, err
	}
	return e, nil
}

// Down stops the stack. When purge is true the data volumes are removed too.
func Down(purge bool) error {
	dir, err := ProjectDir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); os.IsNotExist(err) {
		return nil // nothing was ever brought up
	}
	args := []string{"down", "--remove-orphans"}
	if purge {
		args = append(args, "--volumes")
	}
	if err := compose(dir, args...); err != nil {
		return err
	}
	// Reflect the new state in the discovery file rather than deleting it, so
	// `endpoint`/`status` still report the last-known ports with running=false.
	if e, err := LoadEndpoints(); err == nil {
		e.Running = false
		e.UpdatedAt = nowRFC3339()
		_ = writeEndpoints(dir, e)
	}
	return nil
}

func buildEndpoints(promPort, grafPort int, running bool) *Endpoints {
	return &Endpoints{
		Running: running,
		Prometheus: PrometheusTarget{
			Port:            promPort,
			URL:             fmt.Sprintf("http://localhost:%d", promPort),
			RemoteWriteURL:  fmt.Sprintf("http://localhost:%d%s", promPort, remoteWritePath),
			RemoteWritePath: remoteWritePath,
		},
		Grafana: GrafanaTarget{
			Port:         grafPort,
			URL:          fmt.Sprintf("http://localhost:%d", grafPort),
			DashboardURL: fmt.Sprintf("http://localhost:%d/d/tmm-realtime", grafPort),
		},
		UpdatedAt: nowRFC3339(),
	}
}

func writeEndpoints(dir string, e *Endpoints) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "endpoints.json"), append(b, '\n'), 0o644)
}

func renderCompose(dir string, promPort, grafPort int, adminPassword string) error {
	tmpl, err := template.New("compose").Parse(composeTemplate)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"PrometheusPort":       promPort,
		"GrafanaPort":          grafPort,
		"GrafanaAdminPassword": adminPassword,
	}); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "docker-compose.yml"), buf.Bytes(), 0o644)
}

// compose runs `docker compose` in the project directory.
func compose(dir string, args ...string) error {
	full := append([]string{"compose"}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// containersRunning reports whether both stack containers are up.
func containersRunning() bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", promContainer, grafContainer).Output()
	if err != nil {
		return false
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 2 {
		return false
	}
	return lines[0] == "true" && lines[1] == "true"
}

// publishedPort returns the host port a running container maps to the given
// internal port (e.g. "9090/tcp"), or 0 if it can't be determined.
func publishedPort(container, internal string) int {
	out, err := exec.Command("docker", "port", container, internal).Output()
	if err != nil {
		return 0
	}
	// Output lines look like "0.0.0.0:9491" (and possibly an IPv6 line).
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if i := strings.LastIndex(line, ":"); i >= 0 {
			var p int
			if _, err := fmt.Sscanf(line[i+1:], "%d", &p); err == nil && p > 0 {
				return p
			}
		}
	}
	return 0
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
