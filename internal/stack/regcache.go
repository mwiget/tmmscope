package stack

import (
	"fmt"
	"os/exec"
	"strings"
)

// Registry-cache integration with the companion `regcachectl` tool
// (https://github.com/mwiget/regcachectl). regcachectl runs a local fleet of
// `registry:2` pull-through caches — one container per upstream registry — so
// repeatedly created/destroyed local stacks stop re-pulling the same images.
//
// tmmscope's only host-side image pulls are this compose stack's Prometheus +
// Grafana images, BOTH of which live on docker.io. So tmmscope leverages only
// the docker.io cache: when the fleet is up we rewrite those two image refs to
// pull through it (host:port/prom/prometheus). The sidecar image (ghcr.io) is
// pulled by the CLUSTER's nodes, not by this host, so it is wired by the tool
// that built the cluster (tmmlitectl/ocibnkctl), not here.
//
// The cross-tool contract is the container name + published port below, NOT a
// shared Go dependency — keep it in sync with regcachectl's cache layout.
const (
	// dockerHubCacheContainer is regcachectl's docker.io pull-through cache
	// container. It publishes the cache on the fleet's first host port
	// (port-base, default 5000) and listens on :5000 inside the container.
	dockerHubCacheContainer = "regcache-dockerhub"

	// dockerHubCacheInternalPort is the in-container listen port every
	// regcachectl `registry:2` cache uses; the host-published port is read
	// from `docker port` so a non-default --port-base is honored automatically.
	dockerHubCacheInternalPort = "5000/tcp"
)

// RegistryCacheMode selects how `up` uses the local regcachectl docker.io cache.
type RegistryCacheMode string

const (
	// RegistryCacheAuto pulls through the cache if the fleet is up, else direct.
	RegistryCacheAuto RegistryCacheMode = "auto"
	// RegistryCacheOff always pulls images directly from docker.io.
	RegistryCacheOff RegistryCacheMode = "off"
	// RegistryCacheOn requires the cache; ResolveDockerHubMirror errors if the
	// fleet is not running.
	RegistryCacheOn RegistryCacheMode = "on"
)

// ResolveDockerHubMirror turns a requested cache mode into the host:port prefix
// the compose stack should pull docker.io images through, or "" for a direct
// pull. host is the address THIS host's docker daemon uses to reach the
// published cache (localhost — the fleet binds host ports). In "on" mode a
// missing fleet is an error; in "auto" it silently falls back to a direct pull.
func ResolveDockerHubMirror(mode RegistryCacheMode, host string) (string, error) {
	if host == "" {
		host = "localhost"
	}
	switch mode {
	case RegistryCacheOff:
		return "", nil
	case RegistryCacheAuto, RegistryCacheOn, "":
		mirror := detectDockerHubMirror(host)
		if mirror == "" && mode == RegistryCacheOn {
			return "", fmt.Errorf("registry cache requested but the regcachectl fleet is not running (run 'regcachectl up')")
		}
		return mirror, nil
	default:
		return "", fmt.Errorf("unknown registry cache mode %q (want auto, on, or off)", mode)
	}
}

// detectDockerHubMirror returns host:port for a running regcachectl docker.io
// cache, or "" when the fleet isn't up (or its port can't be read).
func detectDockerHubMirror(host string) string {
	if !containerRunning(dockerHubCacheContainer) {
		return ""
	}
	port := publishedPort(dockerHubCacheContainer, dockerHubCacheInternalPort)
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// dockerHubPrefix turns a resolved mirror (host:port, possibly "") into the
// prefix prepended to a docker.io image ref in the compose template. Empty
// mirror yields "" so the ref stays a plain docker.io pull.
func dockerHubPrefix(mirror string) string {
	if mirror == "" {
		return ""
	}
	return mirror + "/"
}

// containerRunning reports whether a single named container exists and is up.
func containerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}
