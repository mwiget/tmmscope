package stack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readCompose(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func TestDockerHubPrefix(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"localhost:5000": "localhost:5000/",
		"127.0.0.1:5007": "127.0.0.1:5007/",
	}
	for mirror, want := range cases {
		if got := dockerHubPrefix(mirror); got != want {
			t.Errorf("dockerHubPrefix(%q) = %q, want %q", mirror, got, want)
		}
	}
}

func TestResolveDockerHubMirrorOff(t *testing.T) {
	// off never touches docker, so it is hermetic and must return no mirror.
	got, err := ResolveDockerHubMirror(RegistryCacheOff, "localhost")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("off mode returned mirror %q, want empty", got)
	}
}

func TestResolveDockerHubMirrorUnknownMode(t *testing.T) {
	if _, err := ResolveDockerHubMirror(RegistryCacheMode("bogus"), "localhost"); err == nil {
		t.Error("expected error for unknown mode, got nil")
	}
}

// TestRenderComposeMirror checks the rendered compose rewrites both docker.io
// image refs through the mirror, and leaves them untouched when there is none.
func TestRenderComposeMirror(t *testing.T) {
	dir := t.TempDir()
	if err := renderCompose(dir, 9491, 3000, "pw", "localhost:5000"); err != nil {
		t.Fatal(err)
	}
	out := readCompose(t, dir)
	for _, want := range []string{
		"image: localhost:5000/prom/prometheus:v3.1.0",
		"image: localhost:5000/grafana/grafana:11.4.0",
	} {
		if !contains(out, want) {
			t.Errorf("rendered compose missing %q\n%s", want, out)
		}
	}

	if err := renderCompose(dir, 9491, 3000, "pw", ""); err != nil {
		t.Fatal(err)
	}
	out = readCompose(t, dir)
	for _, want := range []string{
		"image: prom/prometheus:v3.1.0",
		"image: grafana/grafana:11.4.0",
	} {
		if !contains(out, want) {
			t.Errorf("direct-pull compose missing %q\n%s", want, out)
		}
	}
}
