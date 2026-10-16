package llm

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxLabelRunes = 64
	maxModels     = 32
)

// ModelInfo is one model the operator lets users choose.
type ModelInfo struct {
	ID    string // the provider's model id
	Label string // what the user sees
}

// Catalog is the operator-approved set of models. Models always holds Default
// first. A model chosen by a user is only used if it is in the catalog.
type Catalog struct {
	Default string
	Models  []ModelInfo
}

func validLabel(l string) bool {
	if strings.TrimSpace(l) == "" || !utf8.ValidString(l) || utf8.RuneCountInString(l) > maxLabelRunes {
		return false
	}
	for _, r := range l {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// NewCatalog builds a catalog from the default model id and the extra models.
func NewCatalog(def string, extra []ModelInfo) (Catalog, error) {
	if !ValidModelID(def) {
		return Catalog{}, errors.New("llm: invalid default model id")
	}
	c := Catalog{Default: def, Models: []ModelInfo{{ID: def, Label: def}}}
	seen := make(map[string]bool, len(extra))
	for _, m := range extra {
		if !ValidModelID(m.ID) {
			return Catalog{}, errors.New("llm: invalid model id in the catalog")
		}
		if seen[m.ID] {
			return Catalog{}, errors.New("llm: duplicate model id in the catalog")
		}
		seen[m.ID] = true
		label := m.Label
		if label == "" {
			label = m.ID
		}
		if !validLabel(label) {
			return Catalog{}, errors.New("llm: invalid model label in the catalog")
		}
		if m.ID == def {
			c.Models[0].Label = label
			continue
		}
		c.Models = append(c.Models, ModelInfo{ID: m.ID, Label: label})
	}
	if len(c.Models) > maxModels {
		return Catalog{}, errors.New("llm: too many models in the catalog")
	}
	return c, nil
}

// Has reports whether id is in the catalog.
func (c Catalog) Has(id string) bool {
	for _, m := range c.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// Resolve returns pref if the catalog contains it, else the default. Use it
// for every stored user preference: a model that was removed, or a value that
// was tampered with, then falls back instead of reaching the provider.
func (c Catalog) Resolve(pref string) string {
	if pref != "" && c.Has(pref) {
		return pref
	}
	return c.Default
}
