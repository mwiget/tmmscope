// Command tmm-stat-webhook is a mutating admission webhook that injects the
// tmm-stat-exporter sidecar into f5-tmm pods at creation time. It exists for
// operator-managed deployments (FLO/BNK), where the f5-tmm pod is owned by the
// F5Tmm operator and a direct Deployment patch is reconciled away: mutating the
// actual pod at admission survives reconciliation — the standard sidecar pattern.
//
// It injects only into pods that carry the shared `f5tmstat` volume and aren't
// already injected. The AdmissionReview wire format is hand-rolled (a tiny,
// stable schema) so this binary needs no k8s.io/* dependencies; the sidecar
// spec is shared with the direct-patch path via internal/inject.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/mwiget/tmmscope/internal/inject"
)

const (
	sidecarName  = "tmm-stat-exporter"
	tmstatVolume = "f5tmstat"
)

func main() {
	listen := flag.String("listen", ":8443", "HTTPS listen address")
	cert := flag.String("tls-cert", "/tls/tls.crt", "server certificate")
	key := flag.String("tls-key", "/tls/tls.key", "server private key")
	image := flag.String("exporter-image", inject.DefaultImage, "exporter sidecar image")
	rwURL := flag.String("remote-write-url", "", "Prometheus remote_write URL the exporter pushes to")
	cluster := flag.String("cluster", "", "cluster label added to every pushed series")
	flag.Parse()

	m := &mutator{opts: inject.Options{Image: *image, RemoteWriteURL: *rwURL, Cluster: *cluster}}
	http.HandleFunc("/mutate", m.handle)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	log.Printf("tmm-stat-webhook: listening on %s, injecting %s (cluster=%s, remote_write=%s)",
		*listen, *image, *cluster, *rwURL)
	log.Fatal((&http.Server{Addr: *listen}).ListenAndServeTLS(*cert, *key))
}

type mutator struct{ opts inject.Options }

// Minimal AdmissionReview wire types (admission.k8s.io/v1).
type admissionReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Request    *admissionRequest  `json:"request,omitempty"`
	Response   *admissionResponse `json:"response,omitempty"`
}

type admissionRequest struct {
	UID    string          `json:"uid"`
	Object json.RawMessage `json:"object"`
}

type admissionResponse struct {
	UID       string  `json:"uid"`
	Allowed   bool    `json:"allowed"`
	Patch     []byte  `json:"patch,omitempty"` // json marshals []byte as base64, as the API expects
	PatchType *string `json:"patchType,omitempty"`
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

func (m *mutator) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var review admissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "bad AdmissionReview", http.StatusBadRequest)
		return
	}
	resp := &admissionResponse{UID: review.Request.UID, Allowed: true}
	if patch := m.patchFor(review.Request.Object); patch != nil {
		pt := "JSONPatch"
		resp.Patch = patch
		resp.PatchType = &pt
	}
	out := admissionReview{APIVersion: review.APIVersion, Kind: review.Kind, Response: resp}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// patchFor returns a JSONPatch adding the sidecar, or nil to leave the pod
// untouched (already injected, or not a tmm pod carrying the tmstat volume).
func (m *mutator) patchFor(raw json.RawMessage) []byte {
	var pod struct {
		Spec struct {
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
			Volumes []struct {
				Name string `json:"name"`
			} `json:"volumes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		return nil
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == sidecarName {
			return nil // already injected
		}
	}
	hasVol := false
	hasDSSM := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == tmstatVolume {
			hasVol = true
		}
		if v.Name == inject.DSSMCertVolume {
			hasDSSM = true
		}
	}
	if !hasVol {
		return nil // not a tmm pod (no shared tmstat segment to read)
	}
	// Mount tmm's DSSM client cert when the pod carries it, so the exporter reads
	// the iRule token counters out of DSSM/Redis. opts is copied so this is
	// per-pod (a pod without DSSM gets no mount referencing a missing volume).
	opts := m.opts
	opts.DSSMCert = hasDSSM
	patch, err := json.Marshal([]patchOp{{Op: "add", Path: "/spec/containers/-", Value: inject.SidecarSpec(opts)}})
	if err != nil {
		return nil
	}
	return patch
}
