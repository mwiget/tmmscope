// Command tmmscope stands up a standalone Prometheus + Grafana real-time TMM
// telemetry stack and reports its live endpoints for producers to push to.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/mwiget/tmmscope/internal/inject"
	"github.com/mwiget/tmmscope/internal/stack"
	"github.com/mwiget/tmmscope/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "up":
		err = cmdUp(args)
	case "down":
		err = cmdDown(args)
	case "status":
		err = cmdStatus(args)
	case "endpoint", "endpoints":
		err = cmdEndpoint(args)
	case "inject":
		err = cmdInject(args)
	case "eject":
		err = cmdEject(args)
	case "open":
		err = cmdOpen(args)
	case "version", "--version", "-v":
		fmt.Println(version.String())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tmmscope — standalone real-time TMM telemetry (Prometheus + Grafana)

Usage:
  tmmscope <command> [flags]

Commands:
  up         Start the Prometheus + Grafana stack (auto-negotiates ports)
  down       Stop the stack (--purge also removes data volumes)
  status     Show whether the stack is running and on which ports
  endpoint   Print the discovery endpoints (--json for machine-readable)
  inject     Inject the tmm-stat-exporter sidecar into a cluster's f5-tmm
  eject      Remove the tmm-stat-exporter sidecar from a cluster's f5-tmm
  open       Open the Grafana dashboard in a browser
  version    Print version information

Run 'tmmscope <command> -h' for command flags.
`)
}

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	promPort := fs.Int("prometheus-port", 0, "host port for Prometheus remote_write (0 = negotiate from 9491)")
	grafPort := fs.Int("grafana-port", 0, "host port for Grafana (0 = negotiate from 3000)")
	pw := fs.String("grafana-password", os.Getenv("TMMSCOPE_GRAFANA_PASSWORD"), "Grafana admin password")
	_ = fs.Parse(args)

	e, err := stack.Up(stack.UpOptions{
		PrometheusPort:       *promPort,
		GrafanaPort:          *grafPort,
		GrafanaAdminPassword: *pw,
	})
	if err != nil {
		return err
	}
	fmt.Println("tmmscope is up.")
	printHuman(e)
	return nil
}

func cmdDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also remove data volumes (drops stored metrics + Grafana state)")
	_ = fs.Parse(args)
	if err := stack.Down(*purge); err != nil {
		return err
	}
	fmt.Println("tmmscope is down.")
	return nil
}

func cmdStatus(args []string) error {
	e, err := stack.Status()
	if err != nil {
		return err
	}
	if !e.Running && e.Prometheus.Port == 0 {
		fmt.Println("tmmscope: not running (never started)")
		return nil
	}
	if e.Running {
		fmt.Println("tmmscope: running")
	} else {
		fmt.Println("tmmscope: stopped (last-known ports below)")
	}
	printHuman(e)
	return nil
}

func cmdEndpoint(args []string) error {
	fs := flag.NewFlagSet("endpoint", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the discovery document as JSON")
	_ = fs.Parse(args)

	e, err := stack.Status()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(e)
	}
	if e.Prometheus.Port == 0 {
		return fmt.Errorf("tmmscope has never been started; run 'tmmscope up'")
	}
	printHuman(e)
	return nil
}

func injectFlags(name string) (*flag.FlagSet, *inject.Options) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	o := &inject.Options{}
	fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "path to kubeconfig (default: kubectl resolution)")
	fs.StringVar(&o.Context, "context", "", "kube context (default: current)")
	fs.StringVar(&o.Namespace, "namespace", "default", "target namespace")
	fs.StringVar(&o.Deployment, "deployment", "f5-tmm", "target f5-tmm Deployment name")
	fs.StringVar(&o.Cluster, "cluster", "", "cluster= label on every series (default: context name)")
	return fs, o
}

func cmdInject(args []string) error {
	fs, o := injectFlags("inject")
	fs.StringVar(&o.RemoteWriteURL, "remote-write-url", "", "full remote_write URL (default: auto-derive from the bnk-edge gateway)")
	fs.StringVar(&o.Image, "image", inject.DefaultImage, "tmm-stat-exporter image")
	_ = fs.Parse(args)

	if o.Cluster == "" {
		o.Cluster = o.Context
	}
	if o.Cluster == "" {
		return fmt.Errorf("could not infer a cluster label; pass --cluster")
	}
	if o.RemoteWriteURL == "" {
		e, err := stack.Status()
		if err != nil || e.Prometheus.Port == 0 {
			return fmt.Errorf("tmmscope is not running; start it with 'tmmscope up' or pass --remote-write-url")
		}
		url, err := inject.DeriveRemoteWriteURL(*o, e.Prometheus.Port)
		if err != nil {
			return err
		}
		o.RemoteWriteURL = url
	}
	fmt.Printf("injecting %s into %s/%s (cluster=%s) → %s\n", o.Image, o.Namespace, o.Deployment, o.Cluster, o.RemoteWriteURL)
	if err := inject.Inject(*o); err != nil {
		return err
	}
	fmt.Println("injected. metrics will appear in Grafana under cluster=" + o.Cluster)
	return nil
}

func cmdEject(args []string) error {
	fs, o := injectFlags("eject")
	_ = fs.Parse(args)
	if err := inject.Eject(*o); err != nil {
		return err
	}
	fmt.Printf("ejected tmm-stat-exporter from %s/%s\n", o.Namespace, o.Deployment)
	return nil
}

func cmdOpen(args []string) error {
	e, err := stack.Status()
	if err != nil {
		return err
	}
	if e.Grafana.Port == 0 {
		return fmt.Errorf("tmmscope has never been started; run 'tmmscope up'")
	}
	url := e.Grafana.DashboardURL
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	default:
		opener = "xdg-open"
	}
	fmt.Println("opening", url)
	return exec.Command(opener, url).Start()
}

func printHuman(e *stack.Endpoints) {
	fmt.Printf("  Grafana:           %s\n", e.Grafana.URL)
	fmt.Printf("  TMM dashboard:     %s\n", e.Grafana.DashboardURL)
	fmt.Printf("  Prometheus:        %s\n", e.Prometheus.URL)
	fmt.Printf("  remote_write port: %d  (producers push to http://<host-gateway>:%d%s)\n",
		e.Prometheus.Port, e.Prometheus.Port, e.Prometheus.RemoteWritePath)
}
