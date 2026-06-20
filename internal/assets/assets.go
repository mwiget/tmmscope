// Package assets embeds the Prometheus + Grafana provisioning files (config,
// datasource, dashboard provider, and the TMM Real-Time dashboard) and writes
// them into a stack project directory on disk so docker compose can mount them.
package assets

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:files
var files embed.FS

// WriteTelemetry materializes the embedded files into <dir>/telemetry/...
// mirroring the layout the compose file mounts:
//
//	<dir>/telemetry/prometheus/prometheus.yml
//	<dir>/telemetry/grafana/provisioning/...
//	<dir>/telemetry/grafana/dashboards/tmm-realtime.json
//
// Existing files are overwritten so an upgraded binary refreshes provisioning.
func WriteTelemetry(dir string) error {
	return fs.WalkDir(files, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("files", p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, "telemetry", rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}
