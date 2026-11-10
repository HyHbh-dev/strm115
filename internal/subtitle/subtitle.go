// Package subtitle parses and serializes subtitle files while preserving
// timing information, cue numbering and inline markup, so that a translator
// only ever sees the spoken text and never has to reconstruct the container.
package subtitle

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Format identifies a supported subtitle container.
type Format string

const (
	FormatSRT Format = "srt"
	FormatVTT Format = "vtt"
	FormatASS Format = "ass"
	FormatSSA Format = "ssa"
	FormatTXT Format = "txt"
)

// Cue is one subtitle entry. Text may contain embedded newlines.
type Cue struct {
	ID         int           // SRT/WebVTT cue number (0 when absent)
	Identifier string        // verbatim WebVTT cue identifier (may be non-numeric)
	Start      time.Duration // inclusive start timestamp
	End        time.Duration // exclusive end timestamp
	Text       string        // spoken text, newlines normalized to "\n"
	Settings   string        // trailing cue settings / SRT coordinates
	AssPrefix  string        // ASS/SSA "Dialogue: layer,start,end,style,...," prefix
}

// File is a parsed subtitle file together with everything needed to write it
// back without losing formatting.
//
// ASS/SSA/TXT use "skeleton" mode: Lines holds the original file split by
// newline and CueLine[i] points at the line holding Cues[i].Text. Only those
// lines are rewritten on encode, which keeps styles, comments and script
// sections byte-for-byte intact.
//
// SRT/VTT use "rebuild" mode: cues are re-serialized from their fields.
type File struct {
	Format  Format
	Cues    []Cue
	Lines   []string // skeleton mode only (nil for SRT/VTT)
	CueLine []int    // skeleton mode only, parallel to Cues
	Header  []string // WebVTT header and NOTE/STYLE/REGION blocks
	HasIDs  bool     // whether cues carry explicit identifiers

	Newline         string // "\r\n" or "\n", preserved on output
	BOM             bool   // preserve a leading UTF-8 BOM
	TrailingNewline bool   // whether the source ended with a newline
}

var (
	utf8BOM = []byte{0xEF, 0xBB, 0xBF}

	// Matches both SRT (comma) and WebVTT (dot) decimal separators, with
	// optional hour field and 1-3 fractional digits.
	reTimeline    = regexp.MustCompile(`^\s*((?:\d{1,3}:)?\d{1,2}:\d{2}[,.]\d{1,3})\s*-->\s*((?:\d{1,3}:)?\d{1,2}:\d{2}[,.]\d{1,3})\s*(.*)$`)
	reASSDialogue = regexp.MustCompile(`(?i)^\s*dialogue\s*:`)
	reVTTSpecial  = regexp.MustCompile(`(?i)^\s*(NOTE|STYLE|REGION)\b`)
)

// Detect determines the container format from the file name and, when the
// extension is missing or unusual, from a few content signatures.
func Detect(name string, data []byte) Format {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "srt":
		return FormatSRT
	case "vtt", "webvtt":
		return FormatVTT
	case "ass":
		return FormatASS
	case "ssa":
		return FormatSSA
	case "txt", "text":
		return FormatTXT
	}

	head := string(data)
	if len(head) > 512 {
		head = head[:512]
	}
	head = strings.TrimPrefix(head, string(utf8BOM))
	switch {
	case regexp.MustCompile(`(?i)^WEBVTT`).MatchString(head):
		return FormatVTT
	case regexp.MustCompile(`(?i)\[Script Info\]`).MatchString(head):
		return FormatASS
	case regexp.MustCompile(`(?im)^\s*Dialogue\s*:`).MatchString(head):
		return FormatASS
	case regexp.MustCompile(`(?m)^\s*\d+\s*$\s*\d{1,3}:\d{2}:\d{2}[,.]\d{1,3}\s*-->`).MatchString(head):
		return FormatSRT
	}
	return FormatTXT
}

// Read parses a subtitle file from disk.
func Read(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(path, data)
}

// Parse parses subtitle bytes. name is used only for format detection.
func Parse(name string, data []byte) (*File, error) {
	return ParseAs(name, data, "")
}

// ParseAs parses subtitle bytes with an explicit format. Pass an empty
// format to auto-detect from the file name and content.
func ParseAs(name string, data []byte, format Format) (*File, error) {
	if format == "" {
		format = Detect(name, data)
	}
	switch format {
	case FormatSRT, FormatVTT, FormatASS, FormatSSA, FormatTXT:
	default:
		return nil, fmt.Errorf("unsupported subtitle format %q (use srt/vtt/ass/txt)", format)
	}
	f := &File{
		Format:          format,
		Newline:         "\n",
		TrailingNewline: len(data) > 0 && data[len(data)-1] == '\n',
	}
	if bytes.HasPrefix(data, utf8BOM) {
		f.BOM = true
		data = data[len(utf8BOM):]
	}
	if bytes.Contains(data, []byte("\r\n")) {
		f.Newline = "\r\n"
	}

	// Internally everything is processed with "\n" line endings; Encode
	// restores the original newline style at the very end.
	src := strings.ReplaceAll(string(data), "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	src = strings.TrimSuffix(src, "\n")

	switch f.Format {
	case FormatSRT:
		return f, parseSRT(f, src)
	case FormatVTT:
		return f, parseVTT(f, src)
	case FormatASS, FormatSSA:
		return f, parseASS(f, src)
	default:
		return f, parseTXT(f, src)
	}
}

// Texts returns the spoken text of every cue, in file order. The returned
// slice is parallel to File.Cues and is intended to be passed to Encode after
// translation.
func (f *File) Texts() []string {
	out := make([]string, len(f.Cues))
	for i := range f.Cues {
		out[i] = f.Cues[i].Text
	}
	return out
}

// LastEnd 是最后一条字幕的结束时间，也就是这份字幕「覆盖到片子的第几分钟」。
//
// 它是判断「这份字幕是不是这部片的」最可靠的一个信号：字幕站会把同名但
// 完全不同的另一部片挂在同一个 IMDb 下（比如 The Asylum 拍的同名山寨片），
// release 名分不出来，但一部 83 分钟的字幕撑不起一部 173 分钟的电影。
func (f *File) LastEnd() time.Duration {
	var end time.Duration
	for _, c := range f.Cues {
		if c.End > end {
			end = c.End
		}
	}
	return end
}

// Encode rebuilds the subtitle file, substituting translated[i] for the text
// of Cues[i]. It never modifies timestamps, ordering or markup outside the
// translated text itself.
func Encode(f *File, translated []string) ([]byte, error) {
	if len(translated) != len(f.Cues) {
		return nil, fmt.Errorf("encode: got %d translations for %d cues", len(translated), len(f.Cues))
	}

	var body string
	if len(f.Lines) > 0 {
		lines := append([]string(nil), f.Lines...)
		for i, cue := range f.Cues {
			idx := f.CueLine[i]
			if idx < 0 || idx >= len(lines) {
				return nil, fmt.Errorf("encode: cue %d points at invalid line %d", i, idx)
			}
			switch f.Format {
			case FormatASS, FormatSSA:
				lines[idx] = cue.AssPrefix + escapeASSText(translated[i])
			default:
				lines[idx] = translated[i]
			}
		}
		body = strings.Join(lines, "\n")
	} else {
		var b strings.Builder
		switch f.Format {
		case FormatVTT:
			if len(f.Header) == 0 {
				b.WriteString("WEBVTT\n\n")
			} else {
				b.WriteString(strings.Join(f.Header, "\n"))
				b.WriteString("\n\n")
			}
		}
		for i, cue := range f.Cues {
			if f.Format == FormatVTT {
				if f.HasIDs {
					if cue.Identifier != "" {
						b.WriteString(cue.Identifier + "\n")
					} else if cue.ID > 0 {
						fmt.Fprintf(&b, "%d\n", cue.ID)
					} else {
						fmt.Fprintf(&b, "%d\n", i+1)
					}
				}
				fmt.Fprintf(&b, "%s --> %s", formatVTTTime(cue.Start), formatVTTTime(cue.End))
			} else { // SRT always carries a cue number
				id := cue.ID
				if id <= 0 {
					id = i + 1
				}
				fmt.Fprintf(&b, "%d\n", id)
				fmt.Fprintf(&b, "%s --> %s", formatSRTTime(cue.Start), formatSRTTime(cue.End))
			}
			if cue.Settings != "" {
				b.WriteString(" " + cue.Settings)
			}
			b.WriteByte('\n')
			b.WriteString(translated[i])
			b.WriteString("\n\n")
		}
		body = strings.TrimRight(b.String(), "\n")
	}

	if f.TrailingNewline {
		body += "\n"
	}
	if f.Newline == "\r\n" {
		body = strings.ReplaceAll(body, "\n", "\r\n")
	}
	out := []byte(body)
	if f.BOM {
		out = append(append([]byte(nil), utf8BOM...), out...)
	}
	return out, nil
}

// DefaultOutputPath produces "<base>.<target><ext>", e.g. movie.zh.srt.
func DefaultOutputPath(input, target string) string {
	ext := filepath.Ext(input)
	base := strings.TrimSuffix(input, ext)
	if base == "" {
		base = input
	}
	return base + "." + target + ext
}

// ---------- timestamp helpers ----------

// parseTimestamp accepts h:mm:ss,mmm / h:mm:ss.mmm / mm:ss.mmm with 1-3
// fractional digits (short forms are right-padded, e.g. ".1" -> 100ms).
func parseTimestamp(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var h, m, sec, ms int64
	var frac string
	idx := strings.LastIndexAny(s, ",.")
	if idx < 0 {
		return 0, fmt.Errorf("timestamp %q has no fractional part", s)
	}
	frac = s[idx+1:]
	clock := s[:idx]
	if len(frac) == 0 || len(frac) > 3 {
		return 0, fmt.Errorf("timestamp %q has invalid fractional part", s)
	}
	for len(frac) < 3 {
		frac += "0"
	}
	ms, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("timestamp %q: %w", s, err)
	}

	parts := strings.Split(clock, ":")
	switch len(parts) {
	case 2: // mm:ss
		m, err = strconv.ParseInt(parts[0], 10, 64)
		if err == nil {
			sec, err = strconv.ParseInt(parts[1], 10, 64)
		}
	case 3: // hh:mm:ss
		h, err = strconv.ParseInt(parts[0], 10, 64)
		if err == nil {
			m, err = strconv.ParseInt(parts[1], 10, 64)
		}
		if err == nil {
			sec, err = strconv.ParseInt(parts[2], 10, 64)
		}
	default:
		return 0, fmt.Errorf("timestamp %q has unexpected shape", s)
	}
	if err != nil {
		return 0, fmt.Errorf("timestamp %q: %w", s, err)
	}
	return time.Duration(h*3600+m*60+sec)*time.Second + time.Duration(ms)*time.Millisecond, nil
}

func formatSRTTime(d time.Duration) string { return formatTime(d, ',') }
func formatVTTTime(d time.Duration) string { return formatTime(d, '.') }

func formatTime(d time.Duration, sep byte) string {
	if d < 0 {
		d = 0
	}
	totalMS := d.Milliseconds()
	h := totalMS / 3_600_000
	m := (totalMS / 60_000) % 60
	s := (totalMS / 1000) % 60
	ms := totalMS % 1000
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", h, m, s, sep, ms)
}

// ---------- shared parsing helpers ----------

// splitBlocks splits on one or more blank lines. Lines are kept verbatim
// except that leading/trailing blank lines are dropped.
func splitBlocks(src string) [][]string {
	var (
		blocks [][]string
		cur    []string
	)
	for _, line := range strings.Split(src, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				blocks = append(blocks, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks
}

func parseTimelineLine(line string) (start, end time.Duration, settings string, ok bool) {
	m := reTimeline.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, "", false
	}
	start, err1 := parseTimestamp(m[1])
	end, err2 := parseTimestamp(m[2])
	if err1 != nil || err2 != nil {
		return 0, 0, "", false
	}
	return start, end, strings.TrimSpace(m[3]), true
}

func parseSRT(f *File, src string) error {
	for _, block := range splitBlocks(src) {
		ti := -1
		for i, line := range block {
			if strings.Contains(line, "-->") {
				ti = i
				break
			}
		}
		if ti < 0 {
			// Stray text block: keep it as a cue without timing so it still
			// gets translated rather than silently disappearing.
			f.Cues = append(f.Cues, Cue{Text: strings.Join(block, "\n")})
			continue
		}
		start, end, settings, ok := parseTimelineLine(block[ti])
		if !ok {
			return fmt.Errorf("invalid SRT timeline %q", block[ti])
		}
		cue := Cue{Start: start, End: end, Settings: settings}
		if ti > 0 {
			if id, err := strconv.Atoi(strings.TrimSpace(block[ti-1])); err == nil {
				cue.ID = id
				f.HasIDs = true
			}
		}
		cue.Text = strings.Join(block[ti+1:], "\n")
		f.Cues = append(f.Cues, cue)
	}
	if len(f.Cues) == 0 {
		return errors.New("no subtitle cues found in SRT input")
	}
	return nil
}

func parseVTT(f *File, src string) error {
	lines := strings.Split(src, "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(lines[0])), "WEBVTT") {
		// The header block runs until the first blank line.
		i := 0
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			f.Header = append(f.Header, lines[i])
			i++
		}
		src = strings.Join(lines[i:], "\n")
	}

	for _, block := range splitBlocks(src) {
		if reVTTSpecial.MatchString(block[0]) {
			f.Header = append(f.Header, block...)
			f.Header = append(f.Header, "")
			continue
		}
		ti := -1
		for i, line := range block {
			if strings.Contains(line, "-->") {
				ti = i
				break
			}
		}
		if ti < 0 {
			f.Cues = append(f.Cues, Cue{Text: strings.Join(block, "\n")})
			continue
		}
		start, end, settings, ok := parseTimelineLine(block[ti])
		if !ok {
			return fmt.Errorf("invalid WebVTT timeline %q", block[ti])
		}
		cue := Cue{Start: start, End: end, Settings: settings}
		if ti > 0 {
			f.HasIDs = true
			cue.Identifier = strings.TrimSpace(block[0])
			if id, err := strconv.Atoi(cue.Identifier); err == nil {
				cue.ID = id
			}
			// Any extra pre-timeline lines are kept as part of the text.
			cue.Text = strings.Join(append(append([]string(nil), block[1:ti]...), block[ti+1:]...), "\n")
		} else {
			cue.Text = strings.Join(block[ti+1:], "\n")
		}
		f.Cues = append(f.Cues, cue)
	}
	if len(f.Cues) == 0 {
		return errors.New("no subtitle cues found in WebVTT input")
	}
	return nil
}

func parseASS(f *File, src string) error {
	f.Lines = strings.Split(src, "\n")
	for i, line := range f.Lines {
		if !reASSDialogue.MatchString(line) {
			continue
		}
		// A Dialogue record has 10 comma-separated fields, and the 10th
		// (Text) may itself contain commas. Locate the 9th comma in the raw
		// line so the prefix is preserved verbatim, including spaces.
		cut := -1
		for n, commas := 0, 0; n < len(line); n++ {
			if line[n] != ',' {
				continue
			}
			commas++
			if commas == 9 {
				cut = n
				break
			}
		}
		if cut < 0 {
			// Malformed record: leave it untouched rather than losing it.
			continue
		}
		text := strings.NewReplacer(`\N`, "\n", `\n`, "\n").Replace(line[cut+1:])
		f.Cues = append(f.Cues, Cue{Text: text, AssPrefix: line[:cut+1]})
		f.CueLine = append(f.CueLine, i)
	}
	if len(f.Cues) == 0 {
		return errors.New("no Dialogue lines found in ASS/SSA input")
	}
	return nil
}

func escapeASSText(text string) string {
	return strings.ReplaceAll(text, "\n", `\N`)
}

func parseTXT(f *File, src string) error {
	f.Lines = strings.Split(src, "\n")
	for i, line := range f.Lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f.Cues = append(f.Cues, Cue{Text: line})
		f.CueLine = append(f.CueLine, i)
	}
	if len(f.Cues) == 0 {
		return errors.New("input contains no text to translate")
	}
	return nil
}
