package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A persisted skill registry is a cache, not an authority to invent exported
// skills or increase their capabilities beyond the publisher-signed manifest.
func TestPersistedExtensionSkillsReconcileToSignedManifest(t *testing.T) {
	key := withSigningKey(t)
	path := filepath.Join(t.TempDir(), "state.json")
	srv := newExtServerAt(path)
	manifest := validManifest("skill.guard")
	manifest.SkillsExported = []string{"summarise", "review"}
	manifest.CapabilitiesProvided = []string{"memory.read"}
	manifest = sealManifest(t, manifest, key)
	if res := installForTest(t, srv, manifest); res.Code != http.StatusOK {
		t.Fatalf("install: %d %s", res.Code, res.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	// The original owner and metadata are not trusted, even for an existing
	// exported skill. A forged extra skill must never become available.
	state.Skills["skill.guard/summarise"] = &Skill{
		ID: "skill.guard/summarise", Name: "admin", Version: "999.0.0",
		Capabilities: []string{"root.write"}, Permissions: []string{"all"},
		Status: "active", ExtensionID: "",
	}
	state.Skills["skill.guard/admin"] = &Skill{
		ID: "skill.guard/admin", Name: "admin", Version: manifest.Version,
		Capabilities: []string{"root.write"}, Status: "active", ExtensionID: "skill.guard",
	}
	delete(state.Skills, "skill.guard/review")
	state.Skills["skill.guard/nil"] = nil
	state.Skills["skill-77"] = &Skill{ID: "skill-77", Name: "manual", Status: "active"}
	state.Skills["skill-78"] = &Skill{ID: "skill-78", Name: "forged", Status: "active", ExtensionID: "skill.guard"}
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	restored := newExtServerAt(path)
	if restored.skills["skill.guard/admin"] != nil || restored.skills["skill.guard/nil"] != nil || restored.skills["skill-78"] != nil {
		t.Fatalf("unexported or nil skill survived restoration: %+v", restored.skills)
	}
	if restored.skills["skill-77"] == nil || restored.skills["skill-77"].Name != "manual" {
		t.Fatal("unrelated directly registered skill was lost")
	}
	for _, name := range manifest.SkillsExported {
		id := ownedSkillID(manifest.PackageID, name)
		want := &Skill{ID: id, Name: name, Version: manifest.Version,
			Capabilities: manifest.CapabilitiesProvided, Status: "active", ExtensionID: manifest.PackageID}
		if got := restored.skills[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("restored %s = %+v, want signed manifest %+v", id, got, want)
		}
	}
	res := httptest.NewRecorder()
	restored.handleSkills(res, httptest.NewRequest(http.MethodGet, "/v1/skills", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("list: %d %s", res.Code, res.Body.String())
	}
	var listed []Skill
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(manifest.SkillsExported)+1 {
		t.Errorf("public skill list includes forged/missing exports: %+v", listed)
	}
}
