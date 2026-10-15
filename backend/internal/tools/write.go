package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
)

const (
	schemaWrite = `{"type":"object","properties":{"path":{"type":"string","description":"Absolute path of the new file in the user's personal space"},"content":{"type":"string","description":"Text content of the file"}},"required":["path","content"],"additionalProperties":false}`
	schemaMove  = `{"type":"object","properties":{"from":{"type":"string","description":"Absolute path of the existing item"},"to":{"type":"string","description":"Absolute path of the new location; it must not exist"}},"required":["from","to"],"additionalProperties":false}`
)

// writable refuses the space root and everything under /.ocis-agent/ (any
// case): only the memory tools may touch the agent's own folder.
func writable(p string) error {
	if p == "/" {
		return refuse("the root of the space cannot be modified")
	}
	if memory.IsReserved(p) {
		return refuse("paths under /.ocis-agent/ are reserved for the agent's memory; use the memory tools")
	}
	return nil
}

// statMaybe stats p; a missing item is (zero, false, nil).
func statMaybe(ctx context.Context, env Env, p string) (ocisclient.Entry, bool, error) {
	e, err := env.Files.Stat(ctx, env.Creds, env.DriveID, p)
	if errors.Is(err, ocisclient.ErrNotFound) {
		return ocisclient.Entry{}, false, nil
	}
	if err != nil {
		return ocisclient.Entry{}, false, mapErr(err, p)
	}
	return e, true, nil
}

// requireParent checks that the parent of p exists and is a folder.
func requireParent(ctx context.Context, env Env, p string) error {
	parent := path.Dir(p)
	e, ok, err := statMaybe(ctx, env, parent)
	if err != nil {
		return err
	}
	if !ok {
		return refuse("the parent folder %s does not exist", parent)
	}
	if !e.Dir {
		return refuse("%s is not a folder", parent)
	}
	return nil
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func parseWrite(env Env, raw json.RawMessage) (writeArgs, error) {
	a, err := parse[writeArgs](raw)
	if err != nil {
		return writeArgs{}, err
	}
	if a.Path, err = cleanPath(a.Path); err != nil {
		return writeArgs{}, err
	}
	if int64(len(a.Content)) > env.limits().MaxWriteBytes {
		return writeArgs{}, invalid("the content is larger than the write limit of %d bytes", env.limits().MaxWriteBytes)
	}
	if strings.ContainsRune(a.Content, 0) {
		return writeArgs{}, invalid("the content must be text")
	}
	return a, nil
}

type moveArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func parseMove(raw json.RawMessage) (moveArgs, error) {
	a, err := parse[moveArgs](raw)
	if err != nil {
		return a, err
	}
	if a.From, err = cleanPath(a.From); err != nil {
		return a, err
	}
	if a.To, err = cleanPath(a.To); err != nil {
		return a, err
	}
	return a, nil
}

// --- files.create_folder ---

type createFolder struct{ base }

// NewCreateFolder returns files.create_folder.
func NewCreateFolder() Tool {
	return createFolder{base{
		name:   "files.create_folder",
		desc:   "Create a folder in the personal space. Needs the user's approval.",
		schema: schemaPath, risk: RiskWrite,
	}}
}

func (createFolder) check(ctx context.Context, env Env, raw json.RawMessage) (string, error) {
	p, err := parsePath(raw)
	if err != nil {
		return "", err
	}
	return p, writable(p)
}

func (t createFolder) Prepare(ctx context.Context, env Env, raw json.RawMessage) (Prepared, error) {
	p, err := t.check(ctx, env, raw)
	if err != nil {
		return Prepared{}, err
	}
	if _, ok, err := statMaybe(ctx, env, p); err != nil {
		return Prepared{}, err
	} else if ok {
		return Prepared{}, refuse("%s already exists", p)
	}
	if err := requireParent(ctx, env, p); err != nil {
		return Prepared{}, err
	}
	return PreviewDoc{Action: "create folder", Paths: []string{p}}.prepared("")
}

func (t createFolder) Execute(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Result, error) {
	p, err := t.check(ctx, env, raw)
	if err != nil {
		return Result{}, err
	}
	switch err := env.Files.Mkcol(ctx, env.Creds, env.DriveID, p); {
	case err == nil:
		return Result{Summary: "created the folder"}, nil
	case errors.Is(err, ocisclient.ErrExists):
		e, ok, serr := statMaybe(ctx, env, p)
		if serr != nil {
			return Result{}, serr
		}
		if ok && e.Dir {
			return Result{Summary: "the folder already exists"}, nil
		}
		return Result{}, refuse("%s already exists and is not a folder", p)
	case errors.Is(err, ocisclient.ErrNotFound):
		return Result{}, refuse("the parent folder of %s does not exist", p)
	default:
		return Result{}, mapErr(err, p)
	}
}

func (t createFolder) Reconcile(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Reconciled, error) {
	p, err := t.check(ctx, env, raw)
	if err != nil {
		return Reconciled{}, err
	}
	e, ok, err := statMaybe(ctx, env, p)
	switch {
	case err != nil:
		return Reconciled{}, err
	case !ok:
		return Reconciled{State: ReconRetry}, nil
	case e.Dir:
		return Reconciled{State: ReconDone, Result: Result{Summary: "the folder already exists"}}, nil
	}
	return Reconciled{State: ReconFailed, Reason: "a file exists at the target"}, nil
}

// --- files.write_new ---

type writeNew struct{ base }

// NewWriteNew returns files.write_new.
func NewWriteNew() Tool {
	return writeNew{base{
		name:   "files.write_new",
		desc:   "Create a new text file in the personal space. Fails if the path already exists. Needs the user's approval.",
		schema: schemaWrite, risk: RiskWrite,
	}}
}

func (writeNew) check(env Env, raw json.RawMessage) (writeArgs, error) {
	a, err := parseWrite(env, raw)
	if err != nil {
		return a, err
	}
	return a, writable(a.Path)
}

func (t writeNew) Prepare(ctx context.Context, env Env, raw json.RawMessage) (Prepared, error) {
	a, err := t.check(env, raw)
	if err != nil {
		return Prepared{}, err
	}
	if _, ok, err := statMaybe(ctx, env, a.Path); err != nil {
		return Prepared{}, err
	} else if ok {
		return Prepared{}, refuse("%s already exists; use files.overwrite to replace it", a.Path)
	}
	if err := requireParent(ctx, env, a.Path); err != nil {
		return Prepared{}, err
	}
	return PreviewDoc{Action: "create file", Paths: []string{a.Path}, Bytes: int64(len(a.Content)), SHA256: sha256Hex([]byte(a.Content))}.prepared("")
}

func (t writeNew) Execute(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Result, error) {
	a, err := t.check(env, raw)
	if err != nil {
		return Result{}, err
	}
	// oCIS ignores If-None-Match on PUT, so the guard against overwriting is
	// this stat; the race between it and the PUT is an accepted residual risk.
	if _, ok, err := statMaybe(ctx, env, a.Path); err != nil {
		return Result{}, err
	} else if ok {
		return Result{}, refuse("%s already exists", a.Path)
	}
	if _, err := env.Files.Put(ctx, env.Creds, env.DriveID, a.Path, []byte(a.Content), ""); err != nil {
		if errors.Is(err, ocisclient.ErrNotFound) {
			return Result{}, refuse("the parent folder of %s does not exist", a.Path)
		}
		return Result{}, mapErr(err, a.Path)
	}
	return Result{Summary: fmt.Sprintf("created the file (%d bytes)", len(a.Content))}, nil
}

func (t writeNew) Reconcile(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Reconciled, error) {
	a, err := t.check(env, raw)
	if err != nil {
		return Reconciled{}, err
	}
	e, ok, err := statMaybe(ctx, env, a.Path)
	switch {
	case err != nil:
		return Reconciled{}, err
	case !ok:
		return Reconciled{State: ReconRetry}, nil
	case e.Dir:
		return Reconciled{State: ReconFailed, Reason: "a folder exists at the target"}, nil
	}
	data, _, err := env.Files.Get(ctx, env.Creds, env.DriveID, a.Path, env.limits().MaxReadBytes)
	if errors.Is(err, ocisclient.ErrTooLarge) {
		return Reconciled{State: ReconFailed, Reason: "a different file exists at the target"}, nil
	}
	if err != nil {
		return Reconciled{}, mapErr(err, a.Path)
	}
	if sha256Hex(data) == sha256Hex([]byte(a.Content)) {
		return Reconciled{State: ReconDone, Result: Result{Summary: fmt.Sprintf("created the file (%d bytes)", len(a.Content))}}, nil
	}
	return Reconciled{State: ReconFailed, Reason: "a different file exists at the target"}, nil
}

// --- files.move and files.copy ---

type moveCopy struct {
	base
	verb   string // "move" or "copy"
	action string // the preview action
	done   string // the result summary
}

// NewMove returns files.move.
func NewMove() Tool {
	return moveCopy{base: base{
		name:   "files.move",
		desc:   "Move or rename a file or folder inside the personal space. The destination must not exist. Needs the user's approval.",
		schema: schemaMove, risk: RiskWrite,
	}, verb: "move", action: "move", done: "moved the item"}
}

// NewCopy returns files.copy.
func NewCopy() Tool {
	return moveCopy{base: base{
		name:   "files.copy",
		desc:   "Copy a file or folder inside the personal space. The destination must not exist. Needs the user's approval.",
		schema: schemaMove, risk: RiskWrite,
	}, verb: "copy", action: "copy", done: "copied the item"}
}

func (moveCopy) check(raw json.RawMessage) (moveArgs, error) {
	a, err := parseMove(raw)
	if err != nil {
		return a, err
	}
	if err := writable(a.From); err != nil {
		return a, err
	}
	if err := writable(a.To); err != nil {
		return a, err
	}
	if a.From == a.To {
		return a, refuse("the source and the destination are the same")
	}
	if strings.HasPrefix(a.To+"/", a.From+"/") {
		return a, refuse("an item cannot be placed inside itself")
	}
	return a, nil
}

func (t moveCopy) Prepare(ctx context.Context, env Env, raw json.RawMessage) (Prepared, error) {
	a, err := t.check(raw)
	if err != nil {
		return Prepared{}, err
	}
	if _, ok, err := statMaybe(ctx, env, a.From); err != nil {
		return Prepared{}, err
	} else if !ok {
		return Prepared{}, refuse("%s does not exist", a.From)
	}
	if _, ok, err := statMaybe(ctx, env, a.To); err != nil {
		return Prepared{}, err
	} else if ok {
		return Prepared{}, refuse("%s already exists", a.To)
	}
	if err := requireParent(ctx, env, a.To); err != nil {
		return Prepared{}, err
	}
	return PreviewDoc{Action: t.action, Paths: []string{a.From, a.To}}.prepared("")
}

func (t moveCopy) Execute(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Result, error) {
	a, err := t.check(raw)
	if err != nil {
		return Result{}, err
	}
	op := env.Files.Move
	if t.verb == "copy" {
		op = env.Files.Copy
	}
	// oCIS enforces Overwrite: F, so an existing destination is never replaced
	switch err := op(ctx, env.Creds, env.DriveID, a.From, a.To); {
	case err == nil:
		return Result{Summary: t.done}, nil
	case errors.Is(err, ocisclient.ErrExists):
		return Result{}, refuse("%s already exists", a.To)
	case errors.Is(err, ocisclient.ErrNotFound):
		return Result{}, refuse("the source or the destination folder does not exist")
	default:
		return Result{}, mapErr(err, a.From)
	}
}

func (t moveCopy) Reconcile(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Reconciled, error) {
	a, err := t.check(raw)
	if err != nil {
		return Reconciled{}, err
	}
	src, srcOK, err := statMaybe(ctx, env, a.From)
	if err != nil {
		return Reconciled{}, err
	}
	dst, dstOK, err := statMaybe(ctx, env, a.To)
	if err != nil {
		return Reconciled{}, err
	}
	done := Reconciled{State: ReconDone, Result: Result{Summary: t.done}}
	if t.verb == "move" {
		switch {
		case !srcOK && dstOK:
			return done, nil
		case srcOK && !dstOK:
			return Reconciled{State: ReconRetry}, nil
		case srcOK && dstOK:
			return Reconciled{State: ReconFailed, Reason: "both the source and the destination exist"}, nil
		}
		return Reconciled{State: ReconFailed, Reason: "neither the source nor the destination exists"}, nil
	}
	switch {
	case !dstOK:
		return Reconciled{State: ReconRetry}, nil
	case srcOK && src.Dir == dst.Dir && (src.Dir || src.Size == dst.Size):
		return done, nil
	}
	return Reconciled{State: ReconFailed, Reason: "something different exists at the destination"}, nil
}
