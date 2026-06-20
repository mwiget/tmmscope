package inject

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"
)

// DefaultWebhookImage is the published webhook image (multi-arch, like the
// exporter). Override with --webhook-image for a locally-built/imported image.
const DefaultWebhookImage = "ghcr.io/mwiget/tmm-stat-webhook:latest"

//go:embed webhook.tmpl.yaml
var webhookManifest string

// Mode picks the injection mechanism.
type Mode string

const (
	ModeAuto    Mode = "auto"
	ModePatch   Mode = "patch"
	ModeWebhook Mode = "webhook"
)

// DetectMode inspects the target Deployment: if it is owned by a controller
// (e.g. the F5Tmm operator), a direct patch would be reconciled away, so the
// webhook path is required. A plain Deployment (tmmlitectl-shape) takes a patch.
func DetectMode(o Options) (Mode, error) {
	args := o.kubectlArgs("get", "deployment", o.Deployment, "-o",
		"jsonpath={.metadata.ownerReferences[?(@.controller==true)].kind}")
	out, err := exec.Command("kubectl", args...).Output()
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", o.Deployment, err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return ModeWebhook, nil
	}
	return ModePatch, nil
}

// InjectWebhook deploys the self-signed-cert mutating webhook and rolls the
// target pods so the sidecar is injected at recreation.
func InjectWebhook(o Options) error {
	certs, err := generateWebhookCerts("tmm-stat-webhook", o.Namespace)
	if err != nil {
		return fmt.Errorf("generating webhook certs: %w", err)
	}
	key, val := o.targetLabel()
	manifest, err := renderWebhook(map[string]any{
		"Namespace":        o.Namespace,
		"WebhookImage":     o.WebhookImage,
		"ExporterImage":    o.Image,
		"ClusterName":      o.Cluster,
		"RemoteWriteURL":   o.RemoteWriteURL,
		"TargetLabelKey":   key,
		"TargetLabelValue": val,
		"Certs":            certs,
	})
	if err != nil {
		return err
	}
	if err := o.kubectlApply(manifest); err != nil {
		return fmt.Errorf("applying webhook manifests: %w", err)
	}
	fmt.Println("waiting for the webhook to become ready...")
	if err := o.kubectl("rollout", "status", "deployment/tmm-stat-webhook", "--timeout=120s"); err != nil {
		return fmt.Errorf("webhook not ready: %w", err)
	}
	fmt.Println("rolling f5-tmm pods to trigger injection...")
	return o.deleteTargetPods()
}

// EjectWebhook removes the webhook and rolls the target pods so they come back
// without the sidecar.
func EjectWebhook(o Options) error {
	_ = o.kubectlClusterScoped("delete", "mutatingwebhookconfiguration",
		"tmm-stat-webhook-"+o.Namespace, "--ignore-not-found")
	_ = o.kubectl("delete", "deployment,service,secret", "-l", "app=tmm-stat-webhook", "--ignore-not-found")
	_ = o.kubectl("delete", "secret", "tmm-stat-webhook-tls", "--ignore-not-found")
	return o.deleteTargetPods()
}

func renderWebhook(data map[string]any) (string, error) {
	tmpl, err := template.New("webhook").Parse(webhookManifest)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// targetLabel returns the pod selector the webhook matches on (app=<deployment>).
func (o Options) targetLabel() (string, string) {
	return "app", o.Deployment
}

func (o Options) deleteTargetPods() error {
	k, v := o.targetLabel()
	return o.kubectl("delete", "pod", "-l", k+"="+v, "--wait=false")
}

func (o Options) kubectlApply(manifest string) error {
	cmd := exec.Command("kubectl", o.kubectlArgs("apply", "-f", "-")...)
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// kubectl runs a namespaced kubectl command, streaming output.
func (o Options) kubectl(args ...string) error {
	cmd := exec.Command("kubectl", o.kubectlArgs(args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// kubectlClusterScoped runs kubectl without the namespace flag (for cluster-
// scoped resources like MutatingWebhookConfiguration).
func (o Options) kubectlClusterScoped(args ...string) error {
	ns := o.Namespace
	o.Namespace = ""
	defer func() { o.Namespace = ns }()
	return o.kubectl(args...)
}
