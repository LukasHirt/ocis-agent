package store

// Trigger kinds of proactive jobs. Their goals are the fixed templates of
// ProactiveGoal; ModelTaskFor maps all of them to ModelTaskProactive.
const (
	TriggerSchedule  = "schedule"   // the periodic sweep
	TriggerFileEvent = "file_event" // a change in a watch folder
	TriggerManual    = "manual"     // the user asked for a review now
)

const (
	goalReview = "Review my recent file changes and open tasks and propose helpful next steps. Only propose actions; do not make changes without approval."
	goalFiles  = "Files changed in my watched folders. Review what changed and propose helpful next steps. Only propose actions; do not make changes without approval."
)

// ProactiveGoal returns the goal of a proactive job of this trigger kind. The
// Goal reaches the model as the user's own instruction, so it is always one of
// these fixed texts and never embeds a path, a file name, memory or model
// output: the agent finds the changes itself with files.recent_changes, whose
// results are fenced as untrusted data. ok is false for any other kind.
func ProactiveGoal(kind string) (goal string, ok bool) {
	switch kind {
	case TriggerSchedule, TriggerManual:
		return goalReview, true
	case TriggerFileEvent:
		return goalFiles, true
	}
	return "", false
}

// IsProactiveGoal reports whether goal is exactly one of the fixed templates.
func IsProactiveGoal(goal string) bool { return goal == goalReview || goal == goalFiles }
