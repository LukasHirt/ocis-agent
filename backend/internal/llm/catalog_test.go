package llm

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNewCatalogPutsTheDefaultFirstAndFillsLabels(t *testing.T) {
	c, err := NewCatalog("gpt-x", []ModelInfo{{ID: "gpt-x-mini", Label: "Fast"}, {ID: "gpt-x", Label: "Balanced"}, {ID: "big"}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	want := []ModelInfo{{ID: "gpt-x", Label: "Balanced"}, {ID: "gpt-x-mini", Label: "Fast"}, {ID: "big", Label: "big"}}
	if c.Default != "gpt-x" || !reflect.DeepEqual(c.Models, want) {
		t.Errorf("catalog = %+v", c)
	}
	d, err := NewCatalog("only", nil)
	if err != nil || len(d.Models) != 1 || d.Models[0] != (ModelInfo{ID: "only", Label: "only"}) {
		t.Errorf("default-only catalog = %+v, %v", d, err)
	}
}

func TestNewCatalogRefusesBadInput(t *testing.T) {
	many := []ModelInfo{}
	for i := 0; i < 32; i++ {
		many = append(many, ModelInfo{ID: fmt.Sprintf("m%d", i)})
	}
	cases := map[string]struct {
		def   string
		extra []ModelInfo
	}{
		"bad default":       {"bad id", nil},
		"empty default":     {"", nil},
		"bad extra id":      {"d", []ModelInfo{{ID: "a b"}}},
		"duplicate extra":   {"d", []ModelInfo{{ID: "a"}, {ID: "a"}}},
		"duplicate default": {"d", []ModelInfo{{ID: "d", Label: "x"}, {ID: "d", Label: "y"}}},
		"long label":        {"d", []ModelInfo{{ID: "a", Label: strings.Repeat("x", 65)}}},
		"control in label":  {"d", []ModelInfo{{ID: "a", Label: "x\ny"}}},
		"bidi in label":     {"d", []ModelInfo{{ID: "a", Label: "x\u202ey"}}},
		"line separator":    {"d", []ModelInfo{{ID: "a", Label: "x\u2028y"}}},
		"invalid utf8":      {"d", []ModelInfo{{ID: "a", Label: "x\xffy"}}},
		"paragraph sep":     {"d", []ModelInfo{{ID: "a", Label: "x\u2029y"}}},
		"blank label":       {"d", []ModelInfo{{ID: "a", Label: " "}}},
		"nbsp-only label":   {"d", []ModelInfo{{ID: "a", Label: "\u00a0"}}},
		"too many":          {"d", many},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := NewCatalog(c.def, c.extra); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := NewCatalog("d", many[:31]); err != nil {
		t.Errorf("32 entries including the default must be accepted: %v", err)
	}
}

func TestCatalogResolveFallsBackToTheDefault(t *testing.T) {
	c, _ := NewCatalog("gpt-x", []ModelInfo{{ID: "fast"}})
	for pref, want := range map[string]string{"": "gpt-x", "fast": "fast", "gpt-x": "gpt-x", "removed": "gpt-x", "FAST": "gpt-x", "fast ": "gpt-x"} {
		if got := c.Resolve(pref); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", pref, got, want)
		}
	}
	if !c.Has("fast") || c.Has("other") || c.Has("") {
		t.Error("Has is wrong")
	}
	if got := (Catalog{}).Resolve("x"); got != "" {
		t.Errorf("zero catalog Resolve = %q", got)
	}
}

func TestNewCatalogAcceptsA64RuneMultibyteLabel(t *testing.T) {
	if _, err := NewCatalog("d", []ModelInfo{{ID: "a", Label: strings.Repeat("é", 64)}}); err != nil {
		t.Errorf("64-rune label refused: %v", err)
	}
}
