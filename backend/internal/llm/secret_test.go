package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSecretIsRedactedEverywhere(t *testing.T) {
	s := Secret("sk-very-secret")
	type holder struct{ Key Secret }
	outputs := []string{
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s),
		fmt.Sprintf("%q", s), fmt.Sprintf("%x", s), fmt.Sprintf("%+v", holder{s}), fmt.Sprintf("%#v", holder{s}),
	}
	b, err := json.Marshal(holder{s})
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(b))
	for _, out := range outputs {
		if strings.Contains(out, "sk-very-secret") {
			t.Errorf("output %q leaks the secret", out)
		}
	}
	if s.Reveal() != "sk-very-secret" {
		t.Error("Reveal must return the value")
	}
}
