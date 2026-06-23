package inject

import "testing"

func TestEphemeralSidecarSpec_DropsResources_KeepsMountAndEnv(t *testing.T) {
	ec := EphemeralSidecarSpec(Options{Cluster: "c1", RemoteWriteURL: "http://h:9491/api/v1/write", Image: "img"})

	if _, ok := ec["resources"]; ok {
		t.Error("ephemeral container must not carry resources (the subresource rejects them)")
	}
	if ec["name"] != sidecarName {
		t.Errorf("name = %v, want %s", ec["name"], sidecarName)
	}
	if ec["image"] != "img" {
		t.Errorf("image = %v, want img", ec["image"])
	}
	// The read-only f5tmstat mount is the whole point — it must survive.
	mounts, ok := ec["volumeMounts"].([]any)
	if !ok || len(mounts) != 1 {
		t.Fatalf("volumeMounts = %v, want one entry", ec["volumeMounts"])
	}
	m := mounts[0].(map[string]any)
	if m["name"] != tmstatVolume || m["readOnly"] != true {
		t.Errorf("mount = %v, want read-only %s", m, tmstatVolume)
	}
	// Env (downward API + labels) is preserved.
	if _, ok := ec["env"].([]any); !ok {
		t.Error("env must be preserved on the ephemeral container")
	}
	// Permanent spec still carries resources (we didn't mutate the shared map).
	if _, ok := SidecarSpec(Options{})["resources"]; !ok {
		t.Error("SidecarSpec must still include resources")
	}
}

func TestHasNamed(t *testing.T) {
	list := []any{
		map[string]any{"name": "f5-tmm"},
		map[string]any{"name": sidecarName},
	}
	if !hasNamed(list, sidecarName) {
		t.Error("hasNamed should find an existing container by name")
	}
	if hasNamed(list, "absent") {
		t.Error("hasNamed should not match a missing name")
	}
	if hasNamed(nil, sidecarName) {
		t.Error("hasNamed(nil) should be false")
	}
	if hasNamed("not-a-list", sidecarName) {
		t.Error("hasNamed on a non-list should be false")
	}
}
