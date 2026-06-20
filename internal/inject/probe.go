package inject

import (
	"fmt"
	"os/exec"
	"strings"
)

// Probe is what tmmscope found at the target: whether an f5-tmm Deployment
// exists, which injection mode fits it, and which cluster it actually is (so the
// user can confirm they're pointed at the right place before anything changes).
type Probe struct {
	Found        bool
	Mode         Mode   // ModePatch (standalone) or ModeWebhook (operator-managed)
	Kind         string // human-readable cluster/tmm shape
	ResourceKind string // "deployment" or "daemonset"
	Owner        string // controller kind owning the resource, if any (e.g. "F5Tmm")
	Context      string // resolved kube context
	Server       string // cluster API server URL
}

// ProbeCluster inspects the target via the ambient (or flagged) kubeconfig:
// resolves the context + API server for display, checks whether the f5-tmm
// Deployment or DaemonSet exists, and classifies it as tmmlite (plain
// Deployment → direct patch) or FLO/BNK (owned by a controller → admission
// webhook, since a direct patch would be reconciled away).
func ProbeCluster(o Options) (Probe, error) {
	p := Probe{Context: o.Context}
	if p.Context == "" {
		if out, err := exec.Command("kubectl", o.configArgs("config", "current-context")...).Output(); err == nil {
			p.Context = strings.TrimSpace(string(out))
		}
	}
	if out, err := exec.Command("kubectl", o.configArgs("config", "view", "--minify",
		"-o", "jsonpath={.clusters[0].cluster.server}")...).Output(); err == nil {
		p.Server = strings.TrimSpace(string(out))
	}

	// Try Deployment first, then DaemonSet.
	resourceKind := "deployment"
	name, err := exec.Command("kubectl", o.kubectlArgs("get", "deployment", o.Deployment,
		"--ignore-not-found", "-o", "jsonpath={.metadata.name}")...).Output()
	if err != nil {
		return p, fmt.Errorf("querying %q in namespace %q (context %q): %w — is the cluster reachable?",
			o.Deployment, o.Namespace, p.Context, err)
	}
	if strings.TrimSpace(string(name)) == "" {
		resourceKind = "daemonset"
		name, err = exec.Command("kubectl", o.kubectlArgs("get", "daemonset", o.Deployment,
			"--ignore-not-found", "-o", "jsonpath={.metadata.name}")...).Output()
		if err != nil {
			return p, fmt.Errorf("querying %q in namespace %q (context %q): %w — is the cluster reachable?",
				o.Deployment, o.Namespace, p.Context, err)
		}
		if strings.TrimSpace(string(name)) == "" {
			return p, nil // no f5-tmm here
		}
	}
	p.Found = true
	p.ResourceKind = resourceKind

	owner, _ := exec.Command("kubectl", o.kubectlArgs("get", resourceKind, o.Deployment,
		"-o", "jsonpath={.metadata.ownerReferences[?(@.controller==true)].kind}")...).Output()
	p.Owner = strings.TrimSpace(string(owner))
	if p.Owner != "" {
		p.Mode = ModeWebhook
		p.Kind = fmt.Sprintf("FLO/BNK (operator-managed, owned by %s)", p.Owner)
	} else {
		p.Mode = ModePatch
		if resourceKind == "daemonset" {
			p.Kind = "standalone DaemonSet"
		} else {
			p.Kind = "tmmlite (standalone Deployment)"
		}
	}
	return p, nil
}
