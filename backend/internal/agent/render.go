package agent

import (
	"strings"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

const (
	maxAttrBytes   = 200
	maxBlockBytes  = 64 << 10
	maxResultBytes = 96 << 10
	omitted        = "[block omitted: too large]"
	unknownNote    = "The outcome of this action is unknown. It may or may not have happened. Do not repeat it; tell the user to verify."
)

var attrReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

func attr(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
	s = strings.ToValidUTF8(s, " ")
	if len(s) > maxAttrBytes {
		cut := maxAttrBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return attrReplacer.Replace(s)
}

func isSpace(b byte) bool {
	return b == ' ' || (b >= '\t' && b <= '\r')
}

// escapeContent writes "&lt;" for every "<" that, ignoring ASCII whitespace and
// slashes after it and ASCII case, is followed by "data".
func escapeContent(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '<' {
			j := i + 1
			for j < len(s) && (isSpace(s[j]) || s[j] == '/') {
				j++
			}
			if j+4 <= len(s) && strings.EqualFold(s[j:j+4], "data") {
				sb.WriteString("&lt;")
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// Fence renders untrusted content as a data block that the content cannot
// close or open.
func Fence(source, path, sha, content string) string {
	return `<data source="` + attr(source) + `" path="` + attr(path) + `" sha256="` + attr(sha) + `">` +
		"\n" + escapeContent(content) + "\n</data>"
}

// RenderResult renders a tool message. truncated lists the Source:Path of
// blocks that were omitted.
func RenderResult(tool string, state string, res tools.Result, reason string) (text string, truncated []string) {
	var sb strings.Builder
	sb.WriteString("Tool result (" + tool + "): " + state)
	if res.Summary != "" {
		sb.WriteString("\n" + res.Summary)
	}
	if reason != "" {
		sb.WriteString("\n" + reason)
	}
	for _, b := range res.Blocks {
		part := omitted
		if len(b.Content) <= maxBlockBytes {
			part = Fence(b.Source, b.Path, b.SHA256, b.Content)
		}
		if part != omitted && sb.Len()+1+len(part) > maxResultBytes {
			part = omitted
		}
		if part == omitted {
			truncated = append(truncated, b.Source+":"+b.Path)
		}
		sb.WriteString("\n" + part)
	}
	if state == "unknown" {
		sb.WriteString("\n" + unknownNote)
	}
	return sb.String(), truncated
}
