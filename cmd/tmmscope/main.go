// Command tmmscope stands up a standalone Prometheus + Grafana real-time TMM
// telemetry stack and reports its live endpoints for producers to push to.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

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
	regCache := fs.String("registry-cache", "auto", "pull stack images through a local regcachectl docker.io cache: auto|on|off")
	regCacheHost := fs.String("registry-cache-host", "localhost", "host the stack uses to reach the regcachectl cache")
	_ = fs.Parse(args)

	mirror, err := stack.ResolveDockerHubMirror(stack.RegistryCacheMode(*regCache), *regCacheHost)
	if err != nil {
		return err
	}
	if mirror != "" {
		fmt.Printf("registry cache: pulling stack images through regcachectl docker.io cache at %s\n", mirror)
	}

	e, err := stack.Up(stack.UpOptions{
		PrometheusPort:       *promPort,
		GrafanaPort:          *grafPort,
		GrafanaAdminPassword: *pw,
		DockerHubMirror:      mirror,
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
	fs.StringVar(&o.Deployment, "deployment", "f5-tmm", "target f5-tmm Deployment/DaemonSet name")
	fs.StringVar(&o.Cluster, "cluster", "", "cluster= label on every series (default: context name)")
	return fs, o
}

func cmdInject(args []string) error {
	fs, o := injectFlags("inject")
	fs.StringVar(&o.RemoteWriteURL, "remote-write-url", "", "full remote_write URL (default: auto-derive from the bnk-edge gateway)")
	fs.StringVar(&o.Image, "image", inject.DefaultImage, "tmm-stat-exporter image")
	fs.StringVar(&o.WebhookImage, "webhook-image", inject.DefaultWebhookImage, "tmm-stat-webhook image (webhook mode)")
	permanent := fs.Bool("permanent", false, "install a durable sidecar (rolls/restarts f5-tmm pods); default is ephemeral, no restart")
	mode := fs.String("mode", "auto", "permanent-injection target: auto|patch|webhook (only used with --permanent)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.BoolVar(yes, "y", false, "skip the confirmation prompt (shorthand)")
	_ = fs.Parse(args)

	// Probe the (ambient or flagged) kubeconfig: identify the cluster and whether
	// it actually runs an f5-tmm before changing anything.
	probe, err := inject.ProbeCluster(*o)
	if err != nil {
		return err
	}
	if !probe.Found {
		return fmt.Errorf("no %q Deployment/DaemonSet in namespace %q on context %q (%s)\n"+
			"is this the right cluster? use --context / --kubeconfig / --namespace / --deployment",
			o.Deployment, o.Namespace, probe.Context, probe.Server)
	}
	o.ResourceKind = probe.ResourceKind
	if o.Cluster == "" {
		o.Cluster = probe.Context
	}
	if o.Cluster == "" {
		return fmt.Errorf("could not infer a cluster label; pass --cluster")
	}

	if o.RemoteWriteURL == "" {
		e, serr := stack.Status()
		if serr != nil || e.Prometheus.Port == 0 {
			return fmt.Errorf("tmmscope is not running; start it with 'tmmscope up' or pass --remote-write-url")
		}
		url, derr := inject.DeriveRemoteWriteURL(*o, e.Prometheus.Port)
		if derr != nil {
			return derr
		}
		o.RemoteWriteURL = url
	}

	fmt.Printf("Detected %s f5-tmm in namespace %q\n", probe.Kind, o.Namespace)
	fmt.Printf("  context:       %s\n", probe.Context)
	fmt.Printf("  cluster (API): %s\n", probe.Server)
	fmt.Printf("  exporter:      %s\n", o.Image)
	fmt.Printf("  stream label:  cluster=%s\n", o.Cluster)
	fmt.Printf("  remote_write:  %s\n", o.RemoteWriteURL)

	if !*permanent {
		// Default: ephemeral container on each live pod — tmm keeps running.
		fmt.Println("  inject mode:   ephemeral (no f5-tmm restart)")
		fmt.Println("  note: ephemeral containers are transient — they don't survive a pod")
		fmt.Println("        restart and aren't re-added automatically. Use --permanent for a")
		fmt.Println("        durable sidecar (which restarts the pod[s]).")
		if !*yes && !confirm("Inject the tmm-stat-exporter as an ephemeral container?") {
			fmt.Println("aborted.")
			return nil
		}
		if err := inject.InjectEphemeral(*o); err != nil {
			return err
		}
		fmt.Println("injected (ephemeral). metrics will appear in Grafana under cluster=" + o.Cluster)
		return nil
	}

	// --permanent: durable sidecar via patch (standalone) or webhook (operator-managed).
	resolved, err := pickMode(*mode, probe)
	if err != nil {
		return err
	}
	fmt.Printf("  inject mode:   permanent / %s\n", resolved)
	if resolved == inject.ModeWebhook {
		fmt.Printf("  webhook:       %s\n", o.WebhookImage)
	}
	fmt.Println("  WARNING: permanent injection rolls (restarts) the f5-tmm pod(s).")
	if !*yes && !confirm("Permanently inject the sidecar and restart f5-tmm pod(s)?") {
		fmt.Println("aborted.")
		return nil
	}
	if resolved == inject.ModeWebhook {
		err = inject.InjectWebhook(*o)
	} else {
		err = inject.Inject(*o)
	}
	if err != nil {
		return err
	}
	fmt.Println("injected (permanent). metrics will appear in Grafana under cluster=" + o.Cluster)
	return nil
}

func cmdEject(args []string) error {
	fs, o := injectFlags("eject")
	permanent := fs.Bool("permanent", false, "eject a durable sidecar (patch/webhook); default clears an ephemeral injection")
	mode := fs.String("mode", "auto", "permanent-eject target: auto|patch|webhook (only used with --permanent)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.BoolVar(yes, "y", false, "skip the confirmation prompt (shorthand)")
	_ = fs.Parse(args)

	probe, err := inject.ProbeCluster(*o)
	if err != nil {
		return err
	}
	o.ResourceKind = probe.ResourceKind

	if !*permanent {
		// Default: clear an ephemeral injection by recreating the pods (the only
		// way to drop an ephemeral container).
		fmt.Printf("Clear ephemeral tmm-stat-exporter from %s/%s\n", o.Namespace, o.Deployment)
		fmt.Printf("  context:       %s\n", probe.Context)
		fmt.Printf("  cluster (API): %s\n", probe.Server)
		fmt.Println("  note: ephemeral containers can't be removed in place — this recreates the f5-tmm pod(s).")
		if !*yes && !confirm("Recreate f5-tmm pod(s) to clear the ephemeral exporter?") {
			fmt.Println("aborted.")
			return nil
		}
		if err := inject.EjectEphemeral(*o); err != nil {
			return err
		}
		fmt.Printf("cleared ephemeral tmm-stat-exporter from %s/%s\n", o.Namespace, o.Deployment)
		return nil
	}

	resolved, err := pickMode(*mode, probe)
	if err != nil {
		return err
	}
	fmt.Printf("Eject tmm-stat-exporter (permanent / %s) from %s/%s\n", resolved, o.Namespace, o.Deployment)
	fmt.Printf("  context:       %s\n", probe.Context)
	fmt.Printf("  cluster (API): %s\n", probe.Server)
	fmt.Println("  WARNING: this rolls (restarts) the f5-tmm pod(s).")
	if !*yes && !confirm("Proceed?") {
		fmt.Println("aborted.")
		return nil
	}

	if resolved == inject.ModeWebhook {
		err = inject.EjectWebhook(*o)
	} else {
		err = inject.Eject(*o)
	}
	if err != nil {
		return err
	}
	fmt.Printf("ejected tmm-stat-exporter (permanent / %s) from %s/%s\n", resolved, o.Namespace, o.Deployment)
	return nil
}

// pickMode resolves --mode: an explicit patch|webhook wins; auto uses the probed
// mode (errors if the probe couldn't classify the target).
func pickMode(mode string, p inject.Probe) (inject.Mode, error) {
	switch inject.Mode(mode) {
	case inject.ModePatch, inject.ModeWebhook:
		return inject.Mode(mode), nil
	case inject.ModeAuto:
		if p.Mode == "" {
			return "", fmt.Errorf("could not determine injection mode; pass --mode patch|webhook")
		}
		return p.Mode, nil
	default:
		return "", fmt.Errorf("invalid --mode %q (auto|patch|webhook)", mode)
	}
}

// confirm prompts for a y/N answer on stdin.
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
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
	// The ports publish on 0.0.0.0 (all host interfaces), so localhost is only a
	// convenience — Grafana/Prometheus are equally reachable at the host's LAN,
	// Tailscale, or any other IP. Spell that out; "localhost" misleads.
	fmt.Printf("  Grafana:           http://localhost:%d   (bound on 0.0.0.0 — also http://<host-ip>:%d)\n",
		e.Grafana.Port, e.Grafana.Port)
	fmt.Printf("  TMM dashboard:     %s\n", e.Grafana.DashboardURL)
	fmt.Printf("  Prometheus:        http://localhost:%d   (bound on 0.0.0.0)\n", e.Prometheus.Port)
	fmt.Printf("  remote_write port: %d  (producers push to http://<host-gateway>:%d%s)\n",
		e.Prometheus.Port, e.Prometheus.Port, e.Prometheus.RemoteWritePath)
}
