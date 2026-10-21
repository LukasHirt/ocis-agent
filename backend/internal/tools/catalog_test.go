package tools

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
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
	want := "files.copy,files.create_folder,files.delete,files.list,files.list_spaces,files.move,files.overwrite,files.read_text,files.recent_changes,files.stat,files.write_new,memory.list,memory.read,memory.write"
	if got != want {
		t.Fatalf("tools = %s\nwant    %s", got, want)
	}
}

// The classification table: the gate plan relies on exactly these risks.
func TestClassificationTable(t *testing.T) {
	want := map[string]Risk{
		"files.list_spaces": RiskRead, "files.list": RiskRead, "files.stat": RiskRead, "files.read_text": RiskRead,
		"files.recent_changes": RiskRead,
		"memory.list":          RiskRead, "memory.read": RiskRead,
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

// With rejected credentials every tool that talks to oCIS must surface
// ErrUnauthorized (so the runner can report it), never a refusal the model
// would read or an invalid-arguments error.
func TestEveryToolSurfacesAnUnauthorizedAnswer(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/a.md", "content")
	f.put("/docs/sub/x.md", "x")
	f.env.Creds = ocisclient.AppToken("einstein", "wrong")
	info := ExecInfo{IdemKey: "k", TargetETag: "\"etag\""}
	type row struct {
		prepare, exec, rec string // arguments; rec empty: the tool does not reconcile
		prepareCalls       bool   // Prepare talks to oCIS
		execCalls          bool
	}
	table := map[string]row{
		"files.list_spaces":    {prepare: `{}`, exec: `{}`},
		"files.list":           {prepare: `{"path":"/docs"}`, exec: `{"path":"/docs"}`, execCalls: true},
		"files.stat":           {prepare: `{"path":"/docs/a.md"}`, exec: `{"path":"/docs/a.md"}`, execCalls: true},
		"files.read_text":      {prepare: `{"path":"/docs/a.md"}`, exec: `{"path":"/docs/a.md"}`, execCalls: true},
		"files.recent_changes": {prepare: `{}`, exec: `{}`, execCalls: true},
		"files.create_folder":  {prepare: `{"path":"/docs/new"}`, exec: `{"path":"/docs/new"}`, rec: `{"path":"/docs/new"}`, prepareCalls: true, execCalls: true},
		"files.write_new":      {prepare: `{"path":"/docs/n.md","content":"c"}`, exec: `{"path":"/docs/n.md","content":"c"}`, rec: `{"path":"/docs/a.md","content":"c"}`, prepareCalls: true, execCalls: true},
		"files.overwrite":      {prepare: `{"path":"/docs/a.md","content":"c"}`, exec: `{"path":"/docs/a.md","content":"c"}`, rec: `{"path":"/docs/a.md","content":"c"}`, prepareCalls: true, execCalls: true},
		"files.move":           {prepare: `{"from":"/docs/a.md","to":"/docs/b.md"}`, exec: `{"from":"/docs/a.md","to":"/docs/b.md"}`, rec: `{"from":"/docs/a.md","to":"/docs/b.md"}`, prepareCalls: true, execCalls: true},
		"files.copy":           {prepare: `{"from":"/docs/a.md","to":"/docs/b.md"}`, exec: `{"from":"/docs/a.md","to":"/docs/b.md"}`, rec: `{"from":"/docs/a.md","to":"/docs/b.md"}`, prepareCalls: true, execCalls: true},
		"files.delete":         {prepare: `{"path":"/docs/a.md"}`, exec: `{"path":"/docs/a.md"}`, rec: `{"path":"/docs/a.md"}`, prepareCalls: true, execCalls: true},
		"memory.list":          {prepare: `{}`, exec: `{}`, execCalls: true},
		"memory.read":          {prepare: `{"name":"profile.md"}`, exec: `{"name":"profile.md"}`, execCalls: true},
		"memory.write":         {prepare: `{"name":"profile.md","content":"c","mode":"append"}`, exec: `{"name":"profile.md","content":"c","mode":"append"}`, rec: `{"name":"profile.md","content":"c","mode":"replace"}`, execCalls: true},
	}
	r := defaultRegistry(t)
	if len(table) != len(r.Names()) {
		t.Fatalf("the table covers %d tools, the catalog has %d", len(table), len(r.Names()))
	}
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ocisclient.ErrUnauthorized) || errors.Is(err, ErrRefused) || errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%s = %v, want ErrUnauthorized only", what, err)
		}
	}
	for _, name := range r.Names() {
		tool, _ := r.Get(name)
		row, ok := table[name]
		if !ok {
			t.Errorf("%s: no sample arguments", name)
			continue
		}
		_, perr := tool.Prepare(bg, f.env, json.RawMessage(row.prepare))
		if row.prepareCalls {
			check(name+" Prepare", perr)
		} else if perr != nil {
			t.Errorf("%s Prepare needs no oCIS call but failed: %v", name, perr)
		}
		_, eerr := tool.Execute(bg, f.env, json.RawMessage(row.exec), info)
		if row.execCalls {
			check(name+" Execute", eerr)
		} else if eerr != nil {
			t.Errorf("%s Execute needs no oCIS call but failed: %v", name, eerr)
		}
		if tool.Risk(nil) != RiskRead {
			rc, isRec := tool.(Reconciler)
			if !isRec || row.rec == "" {
				t.Errorf("%s: a side-effect tool needs a reconcile row", name)
				continue
			}
			_, rerr := rc.Reconcile(bg, f.env, json.RawMessage(row.rec), info)
			check(name+" Reconcile", rerr)
		}
	}
}
