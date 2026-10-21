package store

import (
	"strings"
	"testing"
)

func TestProactiveGoalsAreFixedTemplates(t *testing.T) {
	want := map[string]string{
		TriggerSchedule:  "Review my recent file changes and open tasks and propose helpful next steps. Only propose actions; do not make changes without approval.",
		TriggerFileEvent: "Files changed in my watched folders. Review what changed and propose helpful next steps. Only propose actions; do not make changes without approval.",
		TriggerManual:    "Review my recent file changes and open tasks and propose helpful next steps. Only propose actions; do not make changes without approval.",
	}
	for kind, goal := range want {
		got, ok := ProactiveGoal(kind)
		if !ok || got != goal {
			t.Errorf("ProactiveGoal(%q) = %q, %v; want %q", kind, got, ok, goal)
		}
		if !strings.Contains(got, "propose") || !IsProactiveGoal(got) {
			t.Errorf("%s: the goal must ask for proposals only and be recognised", kind)
		}
		if ModelTaskFor(kind) != ModelTaskProactive {
			t.Errorf("%s: model task %q", kind, ModelTaskFor(kind))
		}
	}
	if TriggerSchedule != "schedule" || TriggerFileEvent != "file_event" || TriggerManual != "manual" {
		t.Fatalf("trigger kinds = %q %q %q", TriggerSchedule, TriggerFileEvent, TriggerManual)
	}
	for _, kind := range []string{"", TriggerChat, "Schedule", "file_event ", "other"} {
		if g, ok := ProactiveGoal(kind); ok || g != "" {
			t.Errorf("ProactiveGoal(%q) = %q, %v; want no goal", kind, g, ok)
		}
	}
	if IsProactiveGoal("Review my files /Documents/evil.md") || IsProactiveGoal("") {
		t.Error("only the exact templates are proactive goals")
	}
}
