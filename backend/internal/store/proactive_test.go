package store

import (
	"strings"
	"testing"
	"time"
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

func TestQuietHours(t *testing.T) {
	at := func(hm string) time.Time {
		tm, err := time.Parse("15:04", hm)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(2026, 10, 25, tm.Hour(), tm.Minute(), 30, 0, time.UTC)
	}
	cases := []struct {
		q    string
		at   string
		want bool
	}{
		{"", "00:00", false},
		{"", "23:59", false},
		// wrapping midnight: the start is quiet, the end is not
		{"22:00-07:00", "21:59", false},
		{"22:00-07:00", "22:00", true},
		{"22:00-07:00", "23:59", true},
		{"22:00-07:00", "00:00", true},
		{"22:00-07:00", "06:59", true},
		{"22:00-07:00", "07:00", false},
		{"22:00-07:00", "12:00", false},
		// within one day
		{"01:00-05:30", "00:59", false},
		{"01:00-05:30", "01:00", true},
		{"01:00-05:30", "05:29", true},
		{"01:00-05:30", "05:30", false},
		{"00:00-23:59", "23:58", true},
		{"00:00-23:59", "23:59", false},
		{"23:59-00:00", "23:59", true},
		{"23:59-00:00", "00:00", false},
		// a corrupt value is quiet: fail closed
		{"nonsense", "12:00", true},
		{"22:00-22:00", "12:00", true},
		{"24:00-07:00", "12:00", true},
	}
	for _, c := range cases {
		if got := InQuietHours(c.q, at(c.at)); got != c.want {
			t.Errorf("InQuietHours(%q, %s) = %v, want %v", c.q, c.at, got, c.want)
		}
	}
	// the time is read in UTC whatever its location
	berlin := time.FixedZone("CEST", 2*3600)
	if !InQuietHours("22:00-07:00", time.Date(2026, 10, 25, 1, 0, 0, 0, berlin)) {
		t.Error("01:00 CEST is 23:00 UTC: quiet")
	}
	if InQuietHours("22:00-07:00", time.Date(2026, 10, 25, 23, 0, 0, 0, berlin)) {
		t.Error("23:00 CEST is 21:00 UTC: not quiet")
	}
	for q, ok := range map[string]bool{
		"": true, "22:00-07:00": true, "00:00-23:59": true,
		"22:00-22:00": false, "24:00-07:00": false, "22:60-07:00": false, "2200-0700": false,
		"22:00 - 07:00": false, "22:00-07:00 ": false, "x2:00-07:00": false, "22:00–07:00": false,
	} {
		if ValidQuietHours(q) != ok {
			t.Errorf("ValidQuietHours(%q) = %v", q, !ok)
		}
	}
}
