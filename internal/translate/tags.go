package translate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	// Inline markup that must survive translation untouched:
	//   ASS override blocks  {\i1} {\an8} {\pos(1,2)}
	//   SRT/WebVTT markup    <i> </font> <v Roger> <c.yellow>
	//   ASS escapes          \N \n \h
	reInlineTag = regexp.MustCompile(`\{\\[^}]*\}|</?[A-Za-z][^>\n]*>|\\[Nnh]`)

	// Placeholders use a distinctive bracket pair so the model can see and
	// copy them, while remaining easy to verify programmatically.
	rePlaceholder = regexp.MustCompile(`⟦\s*(\d+)\s*⟧`)

	// A few common ways models mangle the bracket pair.
	reAltBracket1 = regexp.MustCompile(`\[\[(\d+)\]\]`)
	reAltBracket2 = regexp.MustCompile(`\[#(\d+)#\]`)
	reAltBracket3 = regexp.MustCompile(`〔(\d+)〕`)
)

// ProtectTags replaces inline markup with numbered placeholders so the model
// never has to reproduce (or accidentally translates) the markup itself.
func ProtectTags(text string) (string, []string) {
	var tags []string
	masked := reInlineTag.ReplaceAllStringFunc(text, func(match string) string {
		placeholder := fmt.Sprintf("⟦%d⟧", len(tags))
		tags = append(tags, match)
		return placeholder
	})
	return masked, tags
}

// RestoreTags substitutes placeholders back with the original markup.
// It returns ok=false when any placeholder is missing, duplicated or unknown,
// in which case the caller should re-translate instead of emitting broken tags.
func RestoreTags(text string, tags []string) (string, bool) {
	if len(tags) == 0 {
		return text, true
	}
	text = normalizePlaceholders(text)

	seen := make(map[int]int, len(tags))
	matches := rePlaceholder.FindAllStringSubmatchIndex(text, -1)
	for _, m := range matches {
		n, err := strconv.Atoi(text[m[2]:m[3]])
		if err != nil || n < 0 || n >= len(tags) {
			return "", false
		}
		seen[n]++
	}
	for i := range tags {
		if seen[i] != 1 {
			return "", false
		}
	}

	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(text[last:m[0]])
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		b.WriteString(tags[n])
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String(), true
}

func normalizePlaceholders(s string) string {
	s = reAltBracket1.ReplaceAllString(s, "⟦$1⟧")
	s = reAltBracket2.ReplaceAllString(s, "⟦$1⟧")
	s = reAltBracket3.ReplaceAllString(s, "⟦$1⟧")
	return s
}

// A numbered batch line must stay on a single physical line, so embedded
// newlines are encoded as a literal backslash-n and restored afterwards.
const encodedNewline = `\n`

func encodeBatchLine(s string) string {
	return strings.ReplaceAll(s, "\n", encodedNewline)
}

func decodeBatchLine(s string) string {
	return strings.ReplaceAll(s, encodedNewline, "\n")
}
