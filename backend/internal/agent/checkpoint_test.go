package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

func TestParseCheckpointFresh(t *testing.T) {
	for _, raw := range []string{"", "{}", "null", "{ }", `{"seen":{}}`, `{"messages":null}`} {
		cp, err := ParseCheckpoint(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if cp.V != 1 || cp.Seen == nil || cp.NextCall != 0 {
			t.Fatalf("%q: %+v", raw, cp)
		}
	}
}

func TestParseCheckpointErrors(t *testing.T) {
	for _, raw := range []string{"garbage", `{"v":2}`, `{"v":0,"turns":1}`, `{"v":0}`, `{"turns":3}`, `[1]`} {
		if _, err := ParseCheckpoint(json.RawMessage(raw)); err == nil {
			t.Errorf("%q: want error", raw)
		}
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	cp, _ := ParseCheckpoint(nil)
	cp.Messages = []llm.Message{{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c0", Name: "files.list", Arguments: "{}"}}}}
	cp.Turns = 2
	cp.Note("a")
	cp.Answer = "done"
	raw, err := cp.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cp, got) {
		t.Fatalf("got %+v want %+v", got, cp)
	}
}

func TestNewCallIDSurvivesRoundTrip(t *testing.T) {
	cp, _ := ParseCheckpoint(nil)
	if a, b := cp.NewCallID(), cp.NewCallID(); a != "c0" || b != "c1" {
		t.Fatalf("%s %s", a, b)
	}
	raw, _ := cp.Marshal()
	cp2, err := ParseCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if id := cp2.NewCallID(); id != "c2" {
		t.Fatalf("got %s", id)
	}
}

func asst(ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, llm.ToolCall{ID: id, Name: "t", Arguments: "{}"})
	}
	return m
}

func toolMsg(id string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: id, Content: "x"}
}

func ids(calls []llm.ToolCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.ID)
	}
	return out
}

func TestUnresolved(t *testing.T) {
	user := llm.Message{Role: llm.RoleUser, Content: "q"}
	cases := []struct {
		name string
		msgs []llm.Message
		want []string
	}{
		{"none", nil, nil},
		{"last user", []llm.Message{asst("c0"), user}, nil},
		{"last assistant without calls", []llm.Message{user, {Role: llm.RoleAssistant, Content: "a"}}, nil},
		{"two calls one answered", []llm.Message{user, asst("c0", "c1"), toolMsg("c0")}, []string{"c1"}},
		{"out of order", []llm.Message{user, asst("c0", "c1", "c2"), toolMsg("c2"), toolMsg("c0")}, []string{"c1"}},
		{"all answered", []llm.Message{user, asst("c0"), toolMsg("c0")}, nil},
		{"earlier ignored", []llm.Message{user, asst("c0"), user, asst("c1"), toolMsg("c1")}, nil},
	}
	for _, tc := range cases {
		cp := Checkpoint{V: 1, Messages: tc.msgs}
		if got := ids(cp.Unresolved()); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestNote(t *testing.T) {
	cp, _ := ParseCheckpoint(nil)
	if cp.Note("a") != 1 || cp.Note("a") != 2 || cp.Note("b") != 1 {
		t.Fatal("counting")
	}
	var zero Checkpoint
	if zero.Note("x") != 1 {
		t.Fatal("nil map")
	}
	cp, _ = ParseCheckpoint(nil)
	for i := 0; i < MaxSeenKeys; i++ {
		cp.Note(fmt.Sprintf("k%d", i))
	}
	if n := cp.Note("new"); n != MaxSeenKeys {
		t.Fatalf("new key past cap: %d", n)
	}
	if len(cp.Seen) != MaxSeenKeys {
		t.Fatalf("stored %d", len(cp.Seen))
	}
	if cp.Note("k0") != 2 {
		t.Fatal("existing key still counts")
	}
}

func TestMarshalCap(t *testing.T) {
	cp, _ := ParseCheckpoint(nil)
	cp.Answer = strings.Repeat("a", MaxCheckpointBytes)
	if _, err := cp.Marshal(); err == nil {
		t.Fatal("want error over cap")
	}
}

func TestMarshalDoesNotInflateHTMLChars(t *testing.T) {
	cp, _ := ParseCheckpoint(nil)
	cp.Answer = strings.Repeat("<", 100<<10)
	raw, err := cp.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `\u003c`) || strings.HasSuffix(string(raw), "\n") {
		t.Fatal("escaped html or trailing newline")
	}
	if _, err := ParseCheckpoint(raw); err != nil {
		t.Fatal(err)
	}
}

func TestNoteCallCountsACallIDOnce(t *testing.T) {
	cp, _ := ParseCheckpoint(nil)
	if n := cp.NoteCall("c0", "k"); n != 1 {
		t.Fatalf("first = %d", n)
	}
	if n := cp.NoteCall("c0", "k"); n != 1 {
		t.Fatalf("same call again = %d, want 1", n)
	}
	if n := cp.NoteCall("c1", "k"); n != 2 {
		t.Fatalf("another call = %d, want 2", n)
	}
	raw, err := cp.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cp, got) || !got.Counted["c0"] {
		t.Fatalf("round trip: %+v", got)
	}
	if n := got.NoteCall("c1", "k"); n != 2 {
		t.Fatalf("after round trip = %d, want 2", n)
	}
	if _, err := ParseCheckpoint(json.RawMessage(`{"counted":{"c0":true}}`)); err == nil {
		t.Error("a non-empty checkpoint without a version must be refused")
	}
}

func TestNoteCallIsBounded(t *testing.T) {
	cp, _ := ParseCheckpoint(nil)
	for i := 0; i < MaxSeenKeys+10; i++ {
		cp.NoteCall(fmt.Sprintf("c%d", i), "k")
	}
	if len(cp.Counted) > MaxSeenKeys {
		t.Fatalf("%d counted ids", len(cp.Counted))
	}
}
