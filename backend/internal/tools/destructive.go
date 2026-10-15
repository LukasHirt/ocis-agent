package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
)

var errNoTargetETag = errors.New("tools: this call needs the approved target ETag")

// --- files.overwrite ---

type overwrite struct{ base }

// NewOverwrite returns files.overwrite.
func NewOverwrite() Tool {
	return overwrite{base{
		name:   "files.overwrite",
		desc:   "Replace the content of an existing text file in the personal space. Always needs the user's approval.",
		schema: schemaWrite, risk: RiskDestructive,
	}}
}

func (overwrite) check(env Env, raw json.RawMessage) (writeArgs, error) {
	a, err := parseWrite(env, raw)
	if err != nil {
		return a, err
	}
	return a, writable(a.Path)
}

func (t overwrite) Prepare(ctx context.Context, env Env, raw json.RawMessage) (Prepared, error) {
	a, err := t.check(env, raw)
	if err != nil {
		return Prepared{}, err
	}
	e, ok, err := statMaybe(ctx, env, a.Path)
	switch {
	case err != nil:
		return Prepared{}, err
	case !ok:
		return Prepared{}, refuse("%s does not exist; use files.write_new to create it", a.Path)
	case e.Dir:
		return Prepared{}, refuse("%s is a folder", a.Path)
	}
	return PreviewDoc{
		Action: "overwrite file", Paths: []string{a.Path},
		Bytes: int64(len(a.Content)), SHA256: sha256Hex([]byte(a.Content)),
		Detail: fmt.Sprintf("replaces the current file of %d bytes (ETag %s)", e.Size, e.ETag),
	}.prepared(e.ETag)
}

func (t overwrite) Execute(ctx context.Context, env Env, raw json.RawMessage, info ExecInfo) (Result, error) {
	a, err := t.check(env, raw)
	if err != nil {
		return Result{}, err
	}
	if info.TargetETag == "" {
		return Result{}, errNoTargetETag
	}
	// If-Match is enforced by oCIS on PUT (409 or 412 when the file changed)
	if _, err := env.Files.Put(ctx, env.Creds, env.DriveID, a.Path, []byte(a.Content), info.TargetETag); err != nil {
		if errors.Is(err, ocisclient.ErrPrecondition) {
			return Result{}, changed("%s changed or disappeared after the approval; nothing was overwritten", a.Path)
		}
		return Result{}, mapErr(err, a.Path)
	}
	return Result{Summary: fmt.Sprintf("overwrote the file (%d bytes)", len(a.Content))}, nil
}

func (t overwrite) Reconcile(ctx context.Context, env Env, raw json.RawMessage, info ExecInfo) (Reconciled, error) {
	a, err := t.check(env, raw)
	if err != nil {
		return Reconciled{}, err
	}
	e, ok, err := statMaybe(ctx, env, a.Path)
	switch {
	case err != nil:
		return Reconciled{}, err
	case !ok:
		return Reconciled{State: ReconFailed, Reason: "the file no longer exists"}, nil
	case e.Dir:
		return Reconciled{State: ReconFailed, Reason: "a folder is at the target"}, nil
	}
	data, _, err := env.Files.Get(ctx, env.Creds, env.DriveID, a.Path, env.limits().MaxReadBytes)
	if err == nil && sha256Hex(data) == sha256Hex([]byte(a.Content)) {
		return Reconciled{State: ReconDone, Result: Result{Summary: fmt.Sprintf("overwrote the file (%d bytes)", len(a.Content))}}, nil
	}
	if err != nil && !errors.Is(err, ocisclient.ErrTooLarge) {
		return Reconciled{}, mapErr(err, a.Path)
	}
	if info.TargetETag != "" && e.ETag == info.TargetETag {
		return Reconciled{State: ReconRetry}, nil
	}
	return Reconciled{State: ReconFailed, Reason: "the file changed after the approval"}, nil
}

// --- files.delete ---

type deleteTool struct{ base }

// NewDelete returns files.delete.
func NewDelete() Tool {
	return deleteTool{base{
		name:   "files.delete",
		desc:   "Delete a file or folder of the personal space (it moves to the trash). Always needs the user's approval.",
		schema: schemaPath, risk: RiskDestructive,
	}}
}

func (deleteTool) check(raw json.RawMessage) (string, error) {
	p, err := parsePath(raw)
	if err != nil {
		return "", err
	}
	return p, writable(p)
}

func (t deleteTool) Prepare(ctx context.Context, env Env, raw json.RawMessage) (Prepared, error) {
	p, err := t.check(raw)
	if err != nil {
		return Prepared{}, err
	}
	e, ok, err := statMaybe(ctx, env, p)
	if err != nil {
		return Prepared{}, err
	}
	if !ok {
		return Prepared{}, refuse("%s does not exist", p)
	}
	doc := PreviewDoc{Action: "delete file", Paths: []string{p}, Bytes: e.Size, Detail: fmt.Sprintf("moves it to the trash (ETag %s)", e.ETag)}
	if e.Dir {
		doc.Action = "delete folder"
		doc.Bytes = 0
		doc.Detail = fmt.Sprintf("moves the folder and everything in it to the trash (ETag %s)", e.ETag)
	}
	return doc.prepared(e.ETag)
}

func (t deleteTool) Execute(ctx context.Context, env Env, raw json.RawMessage, info ExecInfo) (Result, error) {
	p, err := t.check(raw)
	if err != nil {
		return Result{}, err
	}
	if info.TargetETag == "" {
		return Result{}, errNoTargetETag
	}
	// oCIS ignores If-Match on DELETE, so the comparison happens here, right
	// before the request. The remaining window is an accepted residual risk;
	// the deletion goes to the trash.
	e, ok, err := statMaybe(ctx, env, p)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{Summary: "the item was already gone"}, nil
	}
	if e.ETag != info.TargetETag {
		return Result{}, changed("%s changed after the approval; nothing was deleted", p)
	}
	if err := env.Files.Delete(ctx, env.Creds, env.DriveID, p); err != nil {
		if errors.Is(err, ocisclient.ErrNotFound) {
			return Result{Summary: "the item was already gone"}, nil
		}
		return Result{}, mapErr(err, p)
	}
	return Result{Summary: "deleted the item (moved to the trash)"}, nil
}

func (t deleteTool) Reconcile(ctx context.Context, env Env, raw json.RawMessage, info ExecInfo) (Reconciled, error) {
	p, err := t.check(raw)
	if err != nil {
		return Reconciled{}, err
	}
	e, ok, err := statMaybe(ctx, env, p)
	switch {
	case err != nil:
		return Reconciled{}, err
	case !ok:
		return Reconciled{State: ReconDone, Result: Result{Summary: "the item was already gone"}}, nil
	case info.TargetETag != "" && e.ETag == info.TargetETag:
		return Reconciled{State: ReconRetry}, nil
	}
	return Reconciled{State: ReconFailed, Reason: "the item changed after the approval"}, nil
}
