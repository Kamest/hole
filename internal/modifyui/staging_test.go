package modifyui

import (
	"reflect"
	"testing"
)

func testRows() []Row {
	return []Row{
		{Entry: "github.com:80,443", Source: "settings"},
		{Entry: "*.npmjs.org:80,443", Source: "agent:claude"},
		{Entry: "api.example.com:80,443", Source: SourceRuntime},
	}
}

func TestStagingRules(t *testing.T) {
	staging := NewStaging(testRows())

	if err := staging.ToggleRemove("github.com:80,443"); err == nil {
		t.Error("a base entry was staged for removal")
	}
	if err := staging.ToggleRemove("api.example.com:80,443"); err != nil {
		t.Fatal(err)
	}
	if err := staging.StageAdd("github.com:80,443"); err == nil {
		t.Error("an entry in force was staged again")
	}
	if err := staging.StageAdd("new.example.org:80,443"); err != nil {
		t.Fatal(err)
	}
	if err := staging.StageAdd("new.example.org:80,443"); err == nil {
		t.Error("an entry was staged twice")
	}
	add, remove := staging.Pending()
	if !reflect.DeepEqual(add, []string{"new.example.org:80,443"}) || !reflect.DeepEqual(remove, []string{"api.example.com:80,443"}) {
		t.Errorf("Pending = %v, %v", add, remove)
	}

	view := staging.View()
	if len(view) != 4 || view[2].Mark != MarkRemove || view[3].Mark != MarkAdd || !view[0].ReadOnly || view[2].ReadOnly {
		t.Errorf("View = %+v", view)
	}

	// Toggling again unstages, and adding back a staged removal cancels it.
	if err := staging.ToggleRemove("new.example.org:80,443"); err != nil {
		t.Fatal(err)
	}
	if err := staging.StageAdd("api.example.com:80,443"); err != nil {
		t.Fatal(err)
	}
	if staging.PendingCount() != 0 {
		t.Errorf("PendingCount = %d after unstaging everything", staging.PendingCount())
	}
}

func TestStagingResetDropsMeaninglessChanges(t *testing.T) {
	staging := NewStaging(testRows())
	_ = staging.ToggleRemove("api.example.com:80,443")
	_ = staging.StageAdd("other.example.org:80,443")
	_ = staging.StageAdd("kept.example.org:80,443")
	// Another `hole modify` removed the runtime entry and added one of the staged ones meanwhile.
	staging.Reset([]Row{
		{Entry: "github.com:80,443", Source: "settings"},
		{Entry: "other.example.org:80,443", Source: SourceRuntime},
	})
	add, remove := staging.Pending()
	if !reflect.DeepEqual(add, []string{"kept.example.org:80,443"}) || len(remove) != 0 {
		t.Errorf("Pending after reset = %v, %v", add, remove)
	}
}

func TestWildcardFor(t *testing.T) {
	tests := []struct {
		name     string
		wildcard string
		offered  bool
	}{
		{"a.b.example.com", "*.b.example.com", true},
		{"cdn.assets.example.co.uk", "*.assets.example.co.uk", true},
		{"x.co.uk", "", false},
		{"api.example.com", "", false},
		{"registry-1.docker.io", "", false},
		{"localhost", "", false},
	}
	for _, test := range tests {
		wildcard, offered := WildcardFor(test.name)
		if wildcard != test.wildcard || offered != test.offered {
			t.Errorf("WildcardFor(%q) = %q, %v; want %q, %v", test.name, wildcard, offered, test.wildcard, test.offered)
		}
	}
}
