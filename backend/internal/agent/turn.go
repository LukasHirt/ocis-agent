package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

const noAnswer = "(no answer)"

// turn makes one model call and records it. stop means the job was released
// (final answer, a limit, a model error); otherwise the new assistant message
// holds tool calls to process, and shown reports whether the streamed text
// before them is still stored on the job as its partial answer (every
// release clears it).
func (r *Runner) turn(ctx context.Context, claim store.Claim, env tools.Env, cp *Checkpoint, shown *bool) (bool, error) {
	if cp.Turns >= r.cfg.MaxSteps {
		// also after a crash right after the turn that reached the limit
		return true, r.fail(ctx, claim, ReasonStepLimit)
	}
	pol, creds, stop, err := r.precheck(ctx, claim, inRun)
	if stop || err != nil {
		return true, err
	}
	env.Creds = creds
	excerpt, err := MemoryExcerpt(ctx, env.Memory, session(env))
	if err != nil {
		return true, err
	}
	req := llm.CompletionRequest{
		Model:     r.d.Catalog.Resolve(pol.Models[store.ModelTaskFor(claim.Job.TriggerKind)]),
		Messages:  append([]llm.Message{{Role: llm.RoleSystem, Content: SystemPrompt(r.cfg.Now()) + excerpt}}, cp.Messages...),
		Tools:     r.specs,
		MaxTokens: r.cfg.MaxTokens,
	}
	resp, p, err := r.complete(ctx, claim, req)
	if err != nil {
		if lost(err) {
			return true, err // the lease went while the answer streamed
		}
		if p != nil && p.stopped() != stopNone && ctx.Err() == nil {
			// a check during the stream cancelled the call
			return true, r.stopStream(ctx, claim, p.stopped(), spentStep(claim, resp))
		}
		// a partial answer of the failed call is cleared by the release
		return true, r.modelError(ctx, claim, err, spentStep(claim, resp))
	}
	step := store.JobStep{
		JobID: claim.Job.ID, Kind: "llm", Status: "ok",
		TokensIn: int64(resp.Usage.In), TokensOut: int64(resp.Usage.Out),
	}
	calls := resp.Message.ToolCalls
	if resp.Stop == llm.StopLength && len(calls) > 0 {
		// the arguments may be truncated: execute nothing
		step.Status = "cut_off"
		return true, r.end(ctx, claim, ending{state: store.JobFailed, reason: ReasonCutOff, step: &step})
	}
	msg := llm.Message{Role: llm.RoleAssistant, Content: resp.Message.Content}
	for _, c := range calls {
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: cp.NewCallID(), Name: c.Name, Arguments: c.Arguments})
	}
	if len(calls) == 0 && msg.Content == "" {
		msg.Content = noAnswer
	}
	cp.Messages = append(cp.Messages, msg)
	cp.Turns++
	cp.Counted = nil // the previous turn's calls are all resolved
	if len(calls) == 0 {
		cp.Answer = msg.Content
	}
	raw, err := cp.Marshal()
	if err != nil {
		return true, r.end(ctx, claim, ending{state: store.JobFailed, reason: ReasonTooLarge, step: &step})
	}
	if len(calls) == 0 {
		// the step, the answer and the release (which clears the partial)
		// in one transaction
		return true, r.end(ctx, claim, ending{state: store.JobSucceeded, checkpoint: raw, step: &step, answer: cp.Answer})
	}
	if p != nil {
		// the calls take a while: show all the text before them meanwhile
		if err := p.final(); err != nil {
			return true, fmt.Errorf("agent: save partial answer: %w", err)
		}
		*shown = p.shown()
	}
	if err := r.d.Store.InTx(ctx, func(tx store.Tx) error {
		if _, err := tx.AppendStep(step, claim.Fence); err != nil {
			return err
		}
		return tx.SaveCheckpoint(claim.Job.UserID, claim.Job.ID, raw, claim.Fence)
	}); err != nil {
		return true, fmt.Errorf("agent: record model turn: %w", err)
	}
	if cp.Turns >= r.cfg.MaxSteps {
		return true, r.fail(ctx, claim, ReasonStepLimit)
	}
	return false, nil
}

// spentStep is the llm step that charges the usage a failed call still
// reports (a stream that had started: its estimate), or nil.
func spentStep(claim store.Claim, resp llm.CompletionResponse) *store.JobStep {
	if resp.Usage.In <= 0 && resp.Usage.Out <= 0 {
		return nil
	}
	return &store.JobStep{
		JobID: claim.Job.ID, Kind: "llm", Status: "failed",
		TokensIn: int64(max(resp.Usage.In, 0)), TokensOut: int64(max(resp.Usage.Out, 0)),
	}
}

// modelError maps a provider error to the job's fate; step (if set) charges
// the failed call's usage in the same transaction. The error text never
// leaves this function: only fixed reasons are recorded.
func (r *Runner) modelError(ctx context.Context, claim store.Claim, err error, step *store.JobStep) error {
	switch {
	case isCtxErr(ctx, err):
		if step != nil {
			// the job stays ours until the caller requeues it: charge now
			// (best effort; a lost lease loses the charge)
			_ = r.d.Store.InTx(context.WithoutCancel(ctx), func(tx store.Tx) error {
				_, err := tx.AppendStep(*step, claim.Fence)
				return err
			})
		}
		return err
	case llm.Retryable(err):
		return r.requeueWith(ctx, claim, requeueDelay(r.cfg.RequeueDelay, claim.Job.Runs), step)
	case errors.Is(err, llm.ErrAuth):
		return r.end(ctx, claim, ending{state: store.JobFailed, reason: ReasonModelAuth, step: step})
	case errors.Is(err, llm.ErrRejected):
		return r.end(ctx, claim, ending{state: store.JobFailed, reason: ReasonModelRejected, step: step})
	}
	return r.end(ctx, claim, ending{state: store.JobFailed, reason: ReasonModelBad, step: step})
}
