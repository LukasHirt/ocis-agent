package agent

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/textcap"
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

var attrReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;",
	"\uff1c", "&lt;", "\ufe64", "&lt;", "\uff1e", "&gt;")

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

// skippable reports whether a rune may be hidden between the opener and the
// letters of "data" or between those letters: every rune that is not a letter
// or number (whitespace, controls, format characters, combining marks,
// variation selectors, punctuation and slash look-alikes), the invisible
// letters U+115F, U+1160, U+3164 and U+FFA0, and U+FFFD or an invalid byte.
func skippable(r rune) bool {
	switch r {
	case utf8.RuneError, 0x115f, 0x1160, 0x3164, 0xffa0:
		return true
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsNumber(r)
}

func isOpener(r rune) bool { return r == '<' || r == '\uff1c' || r == '\ufe64' }

// escapeContent writes "&lt;" for every opener ("<", U+FF1C, U+FE64) that is
// followed, across skippable runes, by the letters "data" (ignoring case) and
// then by a word boundary (not a letter, digit or '_'). Every other "<" is
// left untouched, so code and HTML keep their fidelity. Homoglyph LETTERS of
// "data" and non-'<' bracket look-alikes (angle quotes, numeric entities) are
// an accepted residual risk: they are not our delimiter syntax.
func escapeContent(s string) string {
	runes := make([]rune, 0, len(s))
	offs := make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		runes = append(runes, r)
		offs = append(offs, i)
		i += size
	}
	offs = append(offs, len(s))
	n := len(runes)
	// end[i] is the first non-skippable index at or after i.
	end := make([]int, n+1)
	end[n] = n
	for i := n - 1; i >= 0; i-- {
		if skippable(runes[i]) {
			end[i] = end[i+1]
		} else {
			end[i] = i
		}
	}
	const word = "data"
	matches := func(i int) bool {
		p := end[i+1]
		for k := 0; k < len(word); k++ {
			if p >= n || unicode.ToLower(runes[p]) != rune(word[k]) {
				return false
			}
			p++
			if k < len(word)-1 {
				p = end[p]
			}
		}
		if p < n {
			r := runes[p]
			return !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsNumber(r) || r == '_')
		}
		return true
	}
	var sb strings.Builder
	for i, r := range runes {
		if isOpener(r) && matches(i) {
			sb.WriteString("&lt;")
		} else {
			sb.WriteString(s[offs[i]:offs[i+1]])
		}
	}
	return sb.String()
}

// Fence renders untrusted content as a data block that the content cannot
// close or open.
func Fence(source, path, sha, content string) string {
	return `<data source="` + attr(source) + `" path="` + attr(path) + `" sha256="` + attr(sha) + `">` +
		"\n" + escapeContent(content) + "\n</data>"
}

// RenderResult renders a tool message of at most maxResultBytes in total.
// truncated lists the Source:Path of every block that was not rendered.
func RenderResult(tool string, state string, res tools.Result, reason string) (text string, truncated []string) {
	var sb strings.Builder
	sb.WriteString("Tool result (" + textcap.Cut(tool, 64) + "): " + textcap.Cut(state, 64))
	if res.Summary != "" {
		sb.WriteString("\n" + textcap.Cut(res.Summary, maxSummaryBytes))
	}
	if reason != "" {
		sb.WriteString("\n" + textcap.Cut(reason, maxSummaryBytes))
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
