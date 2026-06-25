package inject

// Ephemeral injection adds the tmm-stat-exporter as an *ephemeral container* to
// each running f5-tmm pod, via the pods/<name>/ephemeralcontainers subresource
// (the mechanism behind `kubectl debug`). Unlike the patch/webhook paths it does
// NOT recreate the pod, so tmm keeps running — `pod.spec.containers` is immutable,
// but ephemeral containers are the one kind you can add to a live pod.
//
// Trade-offs (why this is the default for ad-hoc use but not a permanent install):
//   - transient: an ephemeral container is not restarted if it exits, and is gone
//     when the pod is recreated — nothing re-adds it (no controller behind it).
//   - cannot be removed in place: clearing one requires recreating the pod.
//   - no resources/ports: the subresource rejects those fields, so the exporter
//     runs without a cpu/memory limit here.
//
// It is independent of the patch/webhook axis: because it mutates live pods, it
// works the same on standalone (tmmlite) and operator-managed (FLO/BNK) clusters,
// and the operator won't reconcile it away (it manages the workload, not a pod's
// ephemeralContainers list).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// EphemeralSidecarSpec is SidecarSpec adapted for an ephemeral container: the
// ephemeralcontainers subresource rejects `resources` (and `ports`), so those are
// dropped. Everything else — the f5tmstat mount, downward-API env, and the
// locked-down securityContext — is identical to the permanent sidecar.
func EphemeralSidecarSpec(o Options) map[string]any {
	ec := SidecarSpec(o)
	delete(ec, "resources")
	return ec
}

// InjectEphemeral adds the exporter as an ephemeral container to every running
// f5-tmm pod. It is idempotent: a pod that already carries the exporter (as a
// regular or ephemeral container) is skipped.
func InjectEphemeral(o Options) error {
	pods, err := o.targetPodNames()
	if err != nil {
		return fmt.Errorf("listing f5-tmm pods: %w", err)
	}
	if len(pods) == 0 {
		return fmt.Errorf("no running f5-tmm pods matched app=%s in namespace %q "+
			"(ephemeral injection needs live pods; is tmm running?)", o.Deployment, o.Namespace)
	}
	var added, skipped int
	for _, pod := range pods {
		ok, err := o.injectEphemeralInto(pod)
		if err != nil {
			return fmt.Errorf("pod %s: %w", pod, err)
		}
		if ok {
			added++
			fmt.Printf("  + %s: ephemeral tmm-stat-exporter added\n", pod)
		} else {
			skipped++
			fmt.Printf("  = %s: already instrumented, skipped\n", pod)
		}
	}
	fmt.Printf("ephemeral injection complete: %d added, %d already present\n", added, skipped)
	return nil
}

// EjectEphemeral clears an ephemeral injection. Ephemeral containers cannot be
// removed from a running pod, so the only way to drop them is to recreate the
// pods — which the caller is warned about. The pods come back clean because the
// ephemeral container was never part of the template.
func EjectEphemeral(o Options) error {
	fmt.Println("note: ephemeral containers cannot be removed in place — recreating f5-tmm pod(s) to clear them.")
	return o.deleteTargetPods()
}

// injectEphemeralInto adds the exporter ephemeral container to one pod. Returns
// false (no error) if the pod was already instrumented.
func (o Options) injectEphemeralInto(pod string) (bool, error) {
	raw, err := exec.Command("kubectl", o.kubectlArgs("get", "pod", pod, "-o", "json")...).Output()
	if err != nil {
		return false, fmt.Errorf("get pod: %w", err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, fmt.Errorf("decode pod: %w", err)
	}
	spec, _ := p["spec"].(map[string]any)
	if spec == nil {
		return false, fmt.Errorf("pod has no spec")
	}
	// Idempotency: skip if the exporter is already present either way.
	if hasNamed(spec["containers"], sidecarName) || hasNamed(spec["ephemeralContainers"], sidecarName) {
		return false, nil
	}
	// Refuse pods without the tmstat volume — there'd be nothing to read, and it's
	// the same guard the webhook uses to recognise a real tmm pod.
	if !hasNamed(spec["volumes"], tmstatVolume) {
		return false, fmt.Errorf("pod has no %q volume; not an f5-tmm pod", tmstatVolume)
	}
	// Mount tmm's DSSM client cert when this pod has it, so the exporter can read
	// the iRule token counters out of DSSM/Redis. Detected per-pod so a cluster
	// without DSSM never gets a mount referencing a missing volume.
	o.DSSMCert = hasNamed(spec["volumes"], DSSMCertVolume)

	list, _ := spec["ephemeralContainers"].([]any)
	spec["ephemeralContainers"] = append(list, EphemeralSidecarSpec(o))
	body, err := json.Marshal(p)
	if err != nil {
		return false, err
	}

	// PUT the whole pod back to the ephemeralcontainers subresource; the apiserver
	// honours only the ephemeralContainers change. The namespace is in the path, so
	// use configArgs (no -n) to avoid a redundant/clashing namespace flag.
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/ephemeralcontainers", o.namespace(), pod)
	cmd := exec.Command("kubectl", o.configArgs("replace", "--raw", path, "-f", "-")...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout = io.Discard // the subresource echoes the pod JSON; suppress it
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("add ephemeral container: %w", err)
	}
	return true, nil
}

// targetPodNames lists the running f5-tmm pods (app=<deployment>).
func (o Options) targetPodNames() ([]string, error) {
	k, v := o.targetLabel()
	out, err := exec.Command("kubectl", o.kubectlArgs("get", "pods", "-l", k+"="+v,
		"-o", "jsonpath={.items[*].metadata.name}")...).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(strings.TrimSpace(string(out))), nil
}

// namespace returns the target namespace, defaulting to "default".
func (o Options) namespace() string {
	if o.Namespace == "" {
		return "default"
	}
	return o.Namespace
}

// hasNamed reports whether v is a list of objects containing one with name==name
// (works for containers, ephemeralContainers, and volumes — all keyed on "name").
func hasNamed(v any, name string) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			if n, _ := m["name"].(string); n == name {
				return true
			}
		}
	}
	return false
}
