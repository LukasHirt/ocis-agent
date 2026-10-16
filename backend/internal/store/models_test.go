package store

import (
	"errors"
	"testing"
	"time"
)

func modelsTestNow() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func TestModelTaskFor(t *testing.T) {
	cases := map[string]string{
		TriggerChat:  ModelTaskChat,
		"file_event": ModelTaskProactive,
		"schedule":   ModelTaskProactive,
		"":           ModelTaskProactive,
		"CHAT":       ModelTaskProactive,
	}
	for kind, want := range cases {
		if got := ModelTaskFor(kind); got != want {
			t.Errorf("ModelTaskFor(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestPolicyValidateChecksModelPreferences(t *testing.T) {
	p := DefaultPolicy("u1", modelsTestNow())
	if p.Models == nil || len(p.Models) != 0 {
		t.Fatalf("default Models = %#v, want empty and non-nil", p.Models)
	}
	p.Models = map[string]string{ModelTaskChat: "gpt-x", ModelTaskProactive: "meta-llama/Llama-3.1:8b"}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid preferences: %v", err)
	}
	bad := map[string]map[string]string{
		"unknown task":  {"sweep": "gpt-x"},
		"empty id":      {ModelTaskChat: ""},
		"space in id":   {ModelTaskChat: "a b"},
		"newline in id": {ModelTaskChat: "a\nb"},
		"leading dash":  {ModelTaskChat: "-x"},
	}
	for label, m := range bad {
		q := DefaultPolicy("u1", modelsTestNow())
		q.Models = m
		if err := q.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want an ErrInvalid error", label, err)
		}
	}
}

func TestTriggerChatIsTheChatTriggerKind(t *testing.T) {
	if TriggerChat != "chat" {
		t.Errorf("TriggerChat = %q", TriggerChat)
	}
}
