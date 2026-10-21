package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
)

const (
	schemaNone = `{"type":"object","properties":{},"additionalProperties":false}`
	schemaPath = `{"type":"object","properties":{"path":{"type":"string","description":"Absolute path in the user's personal space, starting with /"}},"required":["path"],"additionalProperties":false}`
)

// base implements the descriptive half of Tool for the tools in this package.
type base struct {
	name, desc, schema string
	risk               Risk
}

func (b base) Name() string              { return b.name }
func (b base) Description() string       { return b.desc }
func (b base) Schema() json.RawMessage   { return json.RawMessage(b.schema) }
func (b base) Risk(json.RawMessage) Risk { return b.risk }
func (b base) AutoAllowed() bool         { return false }

type pathArgs struct {
	Path string `json:"path"`
}

func parsePath(raw json.RawMessage) (string, error) {
	a, err := parse[pathArgs](raw)
	if err != nil {
		return "", err
	}
	return cleanPath(a.Path)
}

// fmtEntry renders one entry as kind, size, modified, etag and path, separated
// by tabs. It goes into a Block: names are untrusted. C0 controls in names
// (newline, tab) are rejected upstream by ocisclient (hrefToPath), and
// visibleName escapes the other characters that break or reorder lines, so a
// name cannot forge extra lines or columns inside the Block.
func fmtEntry(e ocisclient.Entry) string {
	kind := "f"
	if e.Dir {
		kind = "d"
	}
	return fmt.Sprintf("%s\t%d\t%s\t%s\t%s", kind, e.Size, e.ModTime.UTC().Format(time.RFC3339), visibleName(e.ETag), visibleName(e.Path))
}

// mapErr turns an oCIS error into a refusal the model can read, or passes it
// through wrapped (so errors.Is still finds ocisclient.ErrUnauthorized).
func mapErr(err error, what string) error {
	switch {
	case errors.Is(err, ocisclient.ErrNotFound):
		return refuse("%s does not exist", what)
	case errors.Is(err, ocisclient.ErrNotDir):
		return refuse("%s is not a folder", what)
	case errors.Is(err, ocisclient.ErrTooLarge):
		return refuse("%s is too large to process", what)
	case errors.Is(err, ocisclient.ErrForbidden):
		return refuse("access to %s is not allowed", what)
	}
	return fmt.Errorf("tools: %s: %w", what, err)
}

// --- files.list_spaces ---

type listSpaces struct{ base }

// NewListSpaces returns files.list_spaces. The agent can use the personal
// space only, so there is nothing to choose and no id to reveal.
func NewListSpaces() Tool {
	return listSpaces{base{
		name:   "files.list_spaces",
		desc:   "List the spaces the agent can use. In this version that is only the user's personal space.",
		schema: schemaNone, risk: RiskRead,
	}}
}

func (listSpaces) Prepare(_ context.Context, _ Env, raw json.RawMessage) (Prepared, error) {
	_, err := parse[struct{}](raw)
	return Prepared{}, err
}

func (listSpaces) Execute(_ context.Context, _ Env, raw json.RawMessage, _ ExecInfo) (Result, error) {
	if _, err := parse[struct{}](raw); err != nil {
		return Result{}, err
	}
	return Result{Summary: "1 space: the user's personal space (the only space the agent can use)"}, nil
}

// --- files.list ---

type listTool struct{ base }

// NewList returns files.list.
func NewList() Tool {
	return listTool{base{
		name:   "files.list",
		desc:   "List the files and folders directly inside a folder of the personal space.",
		schema: schemaPath, risk: RiskRead,
	}}
}

func (listTool) Prepare(_ context.Context, _ Env, raw json.RawMessage) (Prepared, error) {
	_, err := parsePath(raw)
	return Prepared{}, err
}

func (listTool) Execute(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Result, error) {
	p, err := parsePath(raw)
	if err != nil {
		return Result{}, err
	}
	entries, err := env.Files.List(ctx, env.Creds, env.DriveID, p)
	if err != nil {
		return Result{}, mapErr(err, p)
	}
	total := len(entries)
	limit := env.limits().MaxItems
	summary := fmt.Sprintf("%d item(s)", total)
	if total > limit {
		entries = entries[:limit]
		summary = fmt.Sprintf("%d item(s), truncated (%d in total)", limit, total)
	}
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = fmtEntry(e)
	}
	return Result{Summary: summary, Blocks: []Block{{Source: "listing", Path: p, Content: strings.Join(lines, "\n")}}}, nil
}

// --- files.stat ---

type statTool struct{ base }

// NewStat returns files.stat.
func NewStat() Tool {
	return statTool{base{
		name:   "files.stat",
		desc:   "Show the type, size, modification time and ETag of a file or folder.",
		schema: schemaPath, risk: RiskRead,
	}}
}

func (statTool) Prepare(_ context.Context, _ Env, raw json.RawMessage) (Prepared, error) {
	_, err := parsePath(raw)
	return Prepared{}, err
}

func (statTool) Execute(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Result, error) {
	p, err := parsePath(raw)
	if err != nil {
		return Result{}, err
	}
	e, err := env.Files.Stat(ctx, env.Creds, env.DriveID, p)
	if err != nil {
		return Result{}, mapErr(err, p)
	}
	summary := "folder"
	if !e.Dir {
		summary = fmt.Sprintf("file, %d bytes", e.Size)
	}
	return Result{Summary: summary, Blocks: []Block{{Source: "metadata", Path: p, Content: fmtEntry(e)}}}, nil
}

// --- files.read_text ---

type readText struct{ base }

// NewReadText returns files.read_text.
func NewReadText() Tool {
	return readText{base{
		name:   "files.read_text",
		desc:   "Read a text file of the personal space. Binary and very large files return only their metadata.",
		schema: schemaPath, risk: RiskRead,
	}}
}

func (readText) Prepare(_ context.Context, _ Env, raw json.RawMessage) (Prepared, error) {
	_, err := parsePath(raw)
	return Prepared{}, err
}

func metadataResult(summary string, e ocisclient.Entry) Result {
	return Result{Summary: summary, Blocks: []Block{{Source: "metadata", Path: e.Path, Content: fmtEntry(e)}}}
}

func (readText) Execute(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Result, error) {
	p, err := parsePath(raw)
	if err != nil {
		return Result{}, err
	}
	st, err := env.Files.Stat(ctx, env.Creds, env.DriveID, p)
	if err != nil {
		return Result{}, mapErr(err, p)
	}
	if st.Dir {
		return Result{}, refuse("%s is a folder; use files.list", p)
	}
	limit := env.limits().MaxReadBytes
	if st.Size > limit {
		return metadataResult(fmt.Sprintf("the file is %d bytes, larger than the read limit of %d; metadata only", st.Size, limit), st), nil
	}
	if st.Size > env.Reads.Remaining() {
		return Result{}, refuse("the job's read budget is used up")
	}
	data, _, err := env.Files.Get(ctx, env.Creds, env.DriveID, p, limit)
	if errors.Is(err, ocisclient.ErrTooLarge) { // it grew since the stat
		return metadataResult(fmt.Sprintf("the file is larger than the read limit of %d; metadata only", limit), st), nil
	}
	if err != nil {
		return Result{}, mapErr(err, p)
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return metadataResult("binary file, metadata only", st), nil
	}
	if err := env.Reads.Take(int64(len(data))); err != nil {
		return Result{}, refuse("the job's read budget is used up")
	}
	return Result{
		Summary: fmt.Sprintf("read %d bytes", len(data)),
		Blocks:  []Block{{Source: "file", Path: p, SHA256: sha256Hex(data), Content: string(data)}},
	}, nil
}
