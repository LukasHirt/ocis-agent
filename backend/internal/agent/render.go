package agent

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

const (
	maxAttrBytes    = 200
	maxBlockBytes   = 64 << 10
	maxResultBytes  = 96 << 10
	maxSummaryBytes = 4 << 10
	moreReserve     = 48
	omitted         = "[block omitted: too large]"
	unknownNote     = "The outcome of this action is unknown. It may or may not have happened. Do not repeat it; tell the user to verify."
)

var attrReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// attr cleans an attribute value. The 200-byte limit is on the raw value;
// escaping may grow it afterwards.
func attr(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s) // invalid UTF-8 bytes become U+FFFD here
	if len(s) > maxAttrBytes {
		cut := maxAttrBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return attrReplacer.Replace(s)
}

// skippable reports whether a rune or invalid byte may be hidden between the
// parts of a data tag.
func skippable(r rune, size int) bool {
	return (r == utf8.RuneError && size == 1) || r == '/' || unicode.IsSpace(r) ||
		unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)
}

func isOpener(r rune) bool { return r == '<' || r == '\uff1c' || r == '\ufe64' }

// matchData reports whether s starts with a (skippable)* d (skippable)* a ...
// "data", ignoring case.
func matchData(s string) bool {
	const word = "data"
	k := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if skippable(r, size) {
			i += size
			continue
		}
		if unicode.ToLower(r) != rune(word[k]) {
			return false
		}
		k++
		if k == len(word) {
			return true
		}
		i += size
	}
	return false
}

// escapeContent writes "&lt;" for every opener ("<", U+FF1C, U+FE64) that is,
// after any run of whitespace, slashes, control or format characters and
// invalid bytes, followed by "data" (letters may be separated by the same
// characters), ignoring case. Every other "<" is left untouched.
func escapeContent(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if isOpener(r) && matchData(s[i+size:]) {
			sb.WriteString("&lt;")
		} else {
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	return sb.String()
}

// Fence renders untrusted content as a data block that the content cannot
// close or open.
func Fence(source, path, sha, content string) string {
	return `<data source="` + attr(source) + `" path="` + attr(path) + `" sha256="` + attr(sha) + `">` +
		"\n" + escapeContent(content) + "\n</data>"
}

func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// RenderResult renders a tool message of at most maxResultBytes in total.
// truncated lists the Source:Path of every block that was not rendered.
func RenderResult(tool string, state string, res tools.Result, reason string) (text string, truncated []string) {
	var sb strings.Builder
	sb.WriteString("Tool result (" + tool + "): " + state)
	if res.Summary != "" {
		sb.WriteString("\n" + cutRunes(res.Summary, maxSummaryBytes))
	}
	if reason != "" {
		sb.WriteString("\n" + cutRunes(reason, maxSummaryBytes))
	}
	// Room kept for the fixed tail: the unknown sentence and the final
	// "more blocks omitted" line.
	budget := maxResultBytes - len(unknownNote) - 1 - moreReserve
	more := 0
	for _, b := range res.Blocks {
		key := b.Source + ":" + b.Path
		if more > 0 {
			truncated = append(truncated, key)
			more++
			continue
		}
		part := omitted
		if len(b.Content) <= maxBlockBytes {
			part = Fence(b.Source, b.Path, b.SHA256, b.Content)
			if sb.Len()+1+len(part) > budget {
				part = omitted
			}
		}
		if sb.Len()+1+len(part) > budget {
			truncated = append(truncated, key)
			more = 1
			continue
		}
		if part == omitted {
			truncated = append(truncated, key)
		}
		sb.WriteString("\n" + part)
	}
	if more > 0 {
		sb.WriteString("\n[" + strconv.Itoa(more) + " more blocks omitted]")
	}
	if state == "unknown" {
		sb.WriteString("\n" + unknownNote)
	}
	return sb.String(), truncated
}
