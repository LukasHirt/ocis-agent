package tools

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
)

func defaultRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCatalogHasExactlyTheV1Tools(t *testing.T) {
	got := strings.Join(defaultRegistry(t).Names(), ",")
	want := "files.copy,files.create_folder,files.delete,files.list,files.list_spaces,files.move,files.overwrite,files.read_text,files.stat,files.write_new,memory.list,memory.read,memory.write"
	if got != want {
		t.Fatalf("tools = %s\nwant    %s", got, want)
	}
}

// The classification table: the gate plan relies on exactly these risks.
func TestClassificationTable(t *testing.T) {
	want := map[string]Risk{
		"files.list_spaces": RiskRead, "files.list": RiskRead, "files.stat": RiskRead, "files.read_text": RiskRead,
		"memory.list": RiskRead, "memory.read": RiskRead,
		"files.create_folder": RiskWrite, "files.write_new": RiskWrite, "files.move": RiskWrite, "files.copy": RiskWrite,
		"memory.write":    RiskWrite,
		"files.overwrite": RiskDestructive, "files.delete": RiskDestructive,
	}
	r := defaultRegistry(t)
	for _, name := range r.Names() {
		tool, _ := r.Get(name)
		if got := tool.Risk(nil); got != want[name] {
			t.Errorf("%s: risk %s, want %s", name, got, want[name])
		}
		wantAuto := name == "memory.write"
		if tool.AutoAllowed() != wantAuto {
			t.Errorf("%s: AutoAllowed = %v, want %v", name, tool.AutoAllowed(), wantAuto)
		}
		if tool.Risk(nil) == RiskOutward {
			t.Errorf("%s: no outward tool exists in this version", name)
		}
	}
}

func TestEveryToolWithSideEffectsCanReconcile(t *testing.T) {
	r := defaultRegistry(t)
	for _, name := range r.Names() {
		tool, _ := r.Get(name)
		_, isReconciler := tool.(Reconciler)
		if tool.Risk(nil) != RiskRead && !isReconciler {
			t.Errorf("%s has side effects but cannot reconcile after a crash", name)
		}
	}
}

func TestSchemasAreStrictObjects(t *testing.T) {
	for _, s := range defaultRegistry(t).Specs() {
		var sch struct {
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(s.Schema, &sch); err != nil {
			t.Errorf("%s: invalid schema JSON: %v", s.Name, err)
			continue
		}
		if sch.Type != "object" || sch.AdditionalProperties == nil || *sch.AdditionalProperties {
			t.Errorf("%s: the schema must be an object with additionalProperties false", s.Name)
		}
		for _, req := range sch.Required {
			if _, ok := sch.Properties[req]; !ok {
				t.Errorf("%s: required property %q is not defined", s.Name, req)
			}
		}
		if s.Description == "" || len(s.Description) > 300 {
			t.Errorf("%s: description length %d", s.Name, len(s.Description))
		}
		if _, ok := sch.Properties["drive"]; ok {
			t.Errorf("%s: a tool must not take a drive", s.Name)
		}
	}
}

func TestEveryToolRejectsUnknownFieldsAndNonObjects(t *testing.T) {
	f := newFixture(t)
	r := defaultRegistry(t)
	for _, name := range r.Names() {
		tool, _ := r.Get(name)
		for _, raw := range []string{`[]`, `"x"`, `null`, ``, `{"drive":"other","user":"x","path":"/a"}`} {
			if _, err := tool.Prepare(bg, f.env, json.RawMessage(raw)); !errors.Is(err, ErrInvalidArgs) {
				t.Errorf("%s Prepare(%q) = %v, want ErrInvalidArgs", name, raw, err)
			}
			if _, err := tool.Execute(bg, f.env, json.RawMessage(raw), ExecInfo{}); !errors.Is(err, ErrInvalidArgs) {
				t.Errorf("%s Execute(%q) = %v, want ErrInvalidArgs", name, raw, err)
			}
		}
	}
	if w := writeRequests(f.fake); len(w) != 0 {
		t.Fatalf("invalid calls sent write requests: %v", w)
	}
}

func TestSpecsAreSortedAndCarryTheRisk(t *testing.T) {
	specs := defaultRegistry(t).Specs()
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
		if s.Risk == "unknown" || s.Risk == "" {
			t.Errorf("%s: risk %q", s.Name, s.Risk)
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("specs not sorted: %v", names)
	}
}
