// Package inject adds (and removes) the tmm-stat-exporter sidecar to a target
// cluster's f5-tmm Deployment via a kubectl strategic-merge patch. This is the
// "direct patch" path for non-operator-managed TMM (e.g. tmmlitectl clusters);
// operator-managed FLO/BNK pods need the admission-webhook path instead.
package inject

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DefaultImage is the published exporter image. Override with --image for a
// locally-built/imported image (e.g. tmm-stat-exporter:dev).
const DefaultImage = "ghcr.io/mwiget/tmm-stat-exporter:latest"

const sidecarName = "tmm-stat-exporter"

// Options selects the target and configures the sidecar.
type Options struct {
	Kubeconfig     string // --kubeconfig (empty = default resolution)
	Context        string // --context (empty = current)
	Namespace      string // target namespace (default "default")
	Deployment     string // target deployment (default "f5-tmm")
	Cluster        string // value for the cluster= label on every series
	RemoteWriteURL string // full remote_write URL; empty = auto-derive
	Image          string // exporter image
	WebhookImage   string // webhook image (webhook mode only)
}

// Inject patches the Deployment to add the sidecar. It is idempotent: a
// strategic merge on the container list (merge key "name") updates an existing
// sidecar rather than duplicating it.
func Inject(o Options) error {
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{SidecarSpec(o)},
				},
			},
		},
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return o.patch(string(b))
}

// Eject removes the sidecar via a strategic-merge delete directive.
func Eject(o Options) error {
	patch := `{"spec":{"template":{"spec":{"containers":[{"name":"` + sidecarName + `","$patch":"delete"}]}}}}`
	return o.patch(patch)
}

// SidecarSpec builds the tmm-stat-exporter container as a plain map (so both the
// direct-patch path here and the admission webhook can emit identical specs
// without depending on k8s.io/api). The cluster label and remote_write URL come
// from Options.
func SidecarSpec(o Options) map[string]any {
	labels := fmt.Sprintf("cluster=%s,pod=$(POD_NAME),node=$(NODE_NAME)", o.Cluster)
	return map[string]any{
		"name":            sidecarName,
		"image":           o.Image,
		"imagePullPolicy": "IfNotPresent",
		"env": []any{
			downward("POD_NAME", "metadata.name"),
			downward("NODE_NAME", "spec.nodeName"),
			map[string]any{"name": "TMSTAT_REMOTE_WRITE_URL", "value": o.RemoteWriteURL},
			map[string]any{"name": "TMSTAT_EXTERNAL_LABELS", "value": labels},
		},
		// Locked-down: non-root, read-only rootfs, no caps. Reads the shared
		// tmstat segment read-only and pushes OUTBOUND, so it needs nothing else.
		"securityContext": map[string]any{
			"runAsUser":                int64(65532),
			"runAsGroup":               int64(65532),
			"runAsNonRoot":             true,
			"readOnlyRootFilesystem":   true,
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]any{"drop": []any{"ALL"}},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "50m", "memory": "64Mi"},
			"limits":   map[string]any{"cpu": "50m", "memory": "64Mi"},
		},
		// No readiness/liveness probe: TMM hooks inbound TCP on its dataplane
		// interfaces, so a kubelet probe to the pod IP can't reach the sidecar
		// and would wrongly mark the whole tmm pod NotReady. Telemetry is
		// best-effort and must not gate tmm readiness.
		"volumeMounts": []any{
			map[string]any{"name": "f5tmstat", "mountPath": "/var/tmstat", "readOnly": true},
		},
	}
}

func downward(name, path string) map[string]any {
	return map[string]any{
		"name":      name,
		"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": path}},
	}
}

// patch runs `kubectl patch deployment <name> --type strategic`.
func (o Options) patch(body string) error {
	args := o.kubectlArgs("patch", "deployment", o.Deployment, "--type", "strategic", "-p", body)
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// kubectlArgs prefixes the global selector flags onto a kubectl invocation.
func (o Options) kubectlArgs(args ...string) []string {
	var pre []string
	if o.Kubeconfig != "" {
		pre = append(pre, "--kubeconfig", o.Kubeconfig)
	}
	if o.Context != "" {
		pre = append(pre, "--context", o.Context)
	}
	if o.Namespace != "" {
		pre = append(pre, "-n", o.Namespace)
	}
	return append(pre, args...)
}

// DeriveRemoteWriteURL best-effort computes the push URL for a local
// docker-based tmmlite cluster: it reads the target pod's CNI network-status
// annotation, finds the bnk-edge interface (net1), and uses the .1 host gateway
// of that subnet with the given Prometheus port. Returns an error if it can't
// determine the interface (caller should then require an explicit --remote-write-url).
func DeriveRemoteWriteURL(o Options, port int) (string, error) {
	args := o.kubectlArgs("get", "pods", "-l", "app="+o.Deployment,
		"-o", `jsonpath={.items[0].metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}`)
	out, err := exec.Command("kubectl", args...).Output()
	if err != nil {
		return "", fmt.Errorf("reading pod network-status: %w", err)
	}
	var nets []struct {
		Interface string   `json:"interface"`
		Default   bool     `json:"default"`
		IPs       []string `json:"ips"`
	}
	if err := json.Unmarshal(out, &nets); err != nil {
		return "", fmt.Errorf("parsing network-status (no CNI annotation? pass --remote-write-url): %w", err)
	}
	for _, n := range nets {
		if n.Default || len(n.IPs) == 0 {
			continue
		}
		gw, err := gatewayOf(n.IPs[0])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("http://%s:%d/api/v1/write", gw, port), nil
	}
	return "", fmt.Errorf("no non-default (bnk-edge) interface on the f5-tmm pod; pass --remote-write-url")
}

// gatewayOf returns the .1 address of the /24 the given IPv4 address sits in.
func gatewayOf(ip string) (string, error) {
	parts := strings.Split(strings.TrimSpace(ip), ".")
	if len(parts) != 4 {
		return "", fmt.Errorf("unexpected pod IP %q", ip)
	}
	return fmt.Sprintf("%s.%s.%s.1", parts[0], parts[1], parts[2]), nil
}
