package translate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

const (
	DefaultBatchSize  = 20   // cues per API call
	DefaultBatchChars = 1200 // approx. characters per API call
	DefaultWorkers    = 4
	maxSplitDepth     = 3
)

// Report summarizes a translation run.
type Report struct {
	Total    int
	Batches  int
	Fallback int // cues kept in the source language
	Messages []string
}

// Translated returns the number of cues that received a translation.
func (r *Report) Translated() int { return r.Total - r.Fallback }

// Translator turns a list of subtitle lines into translated lines.
type Translator struct {
	Client *Client

	Target      string // default "zh"
	Source      string // default "auto"
	Instruction string // extra user constraints
	Glossary    string // terminology pairs

	BatchSize  int
	BatchChars int
	Workers    int
	Retries    int

	// OnProgress is called after each completed batch: (done, totalBatches).
	OnProgress func(done, total int)
	// OnWarning receives non-fatal diagnostics as they are produced.
	OnWarning func(msg string)
}

type batchLine struct {
	Original string
	Masked   string
	Tags     []string
}

// BuildBatchPrompt assembles the numbered instTrans prompt for one batch.
// maskedLines contains already protected (and newline-encoded) cue texts.
func (t *Translator) BuildBatchPrompt(maskedLines []string) (string, error) {
	var b strings.Builder
	for i, line := range maskedLines {
		fmt.Fprintf(&b, "%d|%s\n", i+1, line)
	}
	instruction := batchInstruction
	if t.Instruction != "" {
		instruction += "\n" + strings.TrimSpace(t.Instruction)
	}
	return BuildPrompt(strings.TrimSuffix(b.String(), "\n"), t.target(), t.Source, instruction, t.Glossary, "字幕文本")
}

const batchInstruction = "【硬性要求】源文每行以 \"编号|\" 开头；译文必须逐行对应并保留完全相同的编号和竖线，行数、编号、顺序与源文完全一致，不得合并、拆分、增删或重排任何一行\n" +
	"【硬性要求】⟦数字⟧ 是占位标记，必须在译文对应位置原样保留，不得翻译、改写、移动或删除\n" +
	"【硬性要求】源文中的 \\n 是换行标记，必须原样保留为 \\n，不得改成真实换行\n" +
	"【注意】影视字幕翻译：口语化、简洁自然，忠实原意与语气，不要添加解释或注释\n" +
	"【注意】只输出译文，不要输出源文、标题、代码块或任何额外说明"

const singleInstruction = "【硬性要求】⟦数字⟧ 是占位标记，必须原样保留，不得翻译、改写或删除\n" +
	"【硬性要求】源文中的 \\n 是换行标记，必须原样保留\n" +
	"【注意】影视字幕翻译：口语化、简洁自然\n" +
	"【注意】只输出译文，不要有任何额外说明"

func (t *Translator) target() string {
	if t.Target == "" {
		return "zh"
	}
	return t.Target
}

func (t *Translator) workers() int {
	if t.Workers <= 0 {
		return DefaultWorkers
	}
	return t.Workers
}

// BatchCount reports how many API calls Translate would need for texts.
func (t *Translator) BatchCount(texts []string) int {
	return len(t.makeGroups(prepareLines(texts)))
}

// PreviewPrompt returns the exact prompt that would be sent for the first
// batch. Useful for --dry-run without any network access.
func (t *Translator) PreviewPrompt(texts []string) (string, error) {
	lines := prepareLines(texts)
	groups := t.makeGroups(lines)
	if len(groups) == 0 {
		return "", errors.New("no text to translate")
	}
	masked := make([]string, len(groups[0]))
	for k, idx := range groups[0] {
		masked[k] = lines[idx].Masked
	}
	return t.BuildBatchPrompt(masked)
}

func prepareLines(texts []string) []batchLine {
	lines := make([]batchLine, len(texts))
	for i, s := range texts {
		masked, tags := ProtectTags(s)
		lines[i] = batchLine{Original: s, Masked: encodeBatchLine(masked), Tags: tags}
	}
	return lines
}

// makeGroups packs cues into batches bounded by both cue count and character
// count, so long lines never push a request past max_tokens.
func (t *Translator) makeGroups(lines []batchLine) [][]int {
	size := t.BatchSize
	if size <= 0 {
		size = DefaultBatchSize
	}
	maxChars := t.BatchChars
	if maxChars <= 0 {
		maxChars = DefaultBatchChars
	}
	var groups [][]int
	var cur []int
	chars := 0
	for i := range lines {
		cost := utf8.RuneCountInString(lines[i].Masked) + 8 // numbering + newline overhead
		if len(cur) > 0 && (len(cur) >= size || chars+cost > maxChars) {
			groups = append(groups, cur)
			cur = nil
			chars = 0
		}
		cur = append(cur, i)
		chars += cost
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

type batchJob struct {
	indices []int
}

type groupOutcome struct {
	indices  []int
	texts    []string
	warnings []string
	err      error
}

// Translate translates every input line, preserving order. Failures degrade
// gracefully: a misaligned batch is split and retried, and a single cue that
// still fails keeps its original text instead of corrupting the subtitle file.
func (t *Translator) Translate(ctx context.Context, texts []string) ([]string, *Report, error) {
	rep := &Report{Total: len(texts)}
	results := append([]string(nil), texts...)
	lines := prepareLines(texts)
	groups := t.makeGroups(lines)
	rep.Batches = len(groups)
	if len(groups) == 0 {
		return results, rep, nil
	}

	if _, err := ValidateLang(t.target()); err != nil {
		return results, rep, err
	}

	workers := t.workers()
	if workers > len(groups) {
		workers = len(groups)
	}
	jobs := make(chan batchJob)
	outcomes := make(chan groupOutcome, len(groups))
	var wg sync.WaitGroup
	var done int32

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				out, warns, err := t.translateGroup(ctx, job.indices, lines, 0)
				outcomes <- groupOutcome{indices: job.indices, texts: out, warnings: warns, err: err}
				if t.OnProgress != nil {
					t.OnProgress(int(atomic.AddInt32(&done, 1)), len(groups))
				}
			}
		}()
	}
	go func() {
		for _, g := range groups {
			jobs <- batchJob{indices: g}
		}
		close(jobs)
		wg.Wait()
		close(outcomes)
	}()

	var errs []error
	for o := range outcomes {
		for k, idx := range o.indices {
			if k < len(o.texts) {
				results[idx] = o.texts[k]
			}
		}
		for _, msg := range o.warnings {
			rep.Messages = append(rep.Messages, msg)
			if strings.HasPrefix(msg, "[保留原文]") {
				rep.Fallback++
			}
			if t.OnWarning != nil {
				t.OnWarning(msg)
			}
		}
		if o.err != nil {
			errs = append(errs, o.err)
		}
	}
	if err := ctx.Err(); err != nil {
		return results, rep, err
	}
	if len(errs) > 0 && len(errs) == len(groups) {
		return results, rep, fmt.Errorf("all %d batches failed, last error: %w", len(errs), errs[len(errs)-1])
	}
	return results, rep, nil
}

func originals(indices []int, lines []batchLine) []string {
	out := make([]string, len(indices))
	for k, idx := range indices {
		out[k] = lines[idx].Original
	}
	return out
}

// translateGroup attempts a numbered batch, splitting it recursively when the
// response cannot be aligned, and finally falling back to single-cue calls.
func (t *Translator) translateGroup(ctx context.Context, indices []int, lines []batchLine, depth int) ([]string, []string, error) {
	if err := ctx.Err(); err != nil {
		return originals(indices, lines), nil, err
	}
	if len(indices) == 1 {
		text, warns, err := t.translateOne(ctx, indices[0], lines[indices[0]])
		return []string{text}, warns, err
	}

	masked := make([]string, len(indices))
	for k, idx := range indices {
		masked[k] = lines[idx].Masked
	}
	prompt, err := t.BuildBatchPrompt(masked)
	if err != nil {
		return originals(indices, lines), nil, err
	}
	resp, err := t.Client.CompleteWithRetry(ctx, prompt, t.Retries)
	if err == nil {
		if parsed, ok := parseNumberedResponse(resp, len(indices)); ok {
			out := make([]string, len(parsed))
			var warnings []string
			var firstErr error
			for k, raw := range parsed {
				idx := indices[k]
				text, ok := finalize(raw, lines[idx])
				if ok {
					out[k] = text
					continue
				}
				// Placeholder damage on one line: retry just that cue.
				single, ws, e := t.translateOne(ctx, idx, lines[idx])
				out[k] = single
				warnings = append(warnings, ws...)
				if e != nil && firstErr == nil {
					firstErr = e
				}
			}
			return out, warnings, firstErr
		}
		err = errors.New("response lines could not be aligned to source cues")
	}

	if depth >= maxSplitDepth || len(indices) <= 2 {
		out := make([]string, len(indices))
		var warnings []string
		var firstErr error
		for k, idx := range indices {
			text, ws, e := t.translateOne(ctx, idx, lines[idx])
			out[k] = text
			warnings = append(warnings, ws...)
			if e != nil && firstErr == nil {
				firstErr = e
			}
		}
		return out, warnings, firstErr
	}

	mid := len(indices) / 2
	left, wl, el := t.translateGroup(ctx, indices[:mid], lines, depth+1)
	right, wr, er := t.translateGroup(ctx, indices[mid:], lines, depth+1)
	out := append(append([]string(nil), left...), right...)
	warnings := append(append([]string(nil), wl...), wr...)
	if el != nil {
		return out, warnings, el
	}
	return out, warnings, er
}

// translateOne translates a single cue; on failure it keeps the original text.
func (t *Translator) translateOne(ctx context.Context, idx int, line batchLine) (string, []string, error) {
	instruction := singleInstruction
	if t.Instruction != "" {
		instruction += "\n" + strings.TrimSpace(t.Instruction)
	}
	prompt, err := BuildPrompt(line.Masked, t.target(), t.Source, instruction, t.Glossary, "字幕文本")
	if err != nil {
		return line.Original, nil, err
	}
	resp, err := t.Client.CompleteWithRetry(ctx, prompt, t.Retries)
	if err == nil {
		if text, ok := finalize(cleanSingle(resp), line); ok {
			return text, nil, nil
		}
		// One more attempt with the minimal, unconstrained prompt.
		plain, perr := BuildPrompt(line.Masked, t.target(), t.Source, "", t.Glossary, "文本")
		if perr == nil {
			if resp2, err2 := t.Client.CompleteWithRetry(ctx, plain, t.Retries); err2 == nil {
				if text, ok := finalize(cleanSingle(resp2), line); ok {
					return text, nil, nil
				}
			} else {
				err = err2
			}
		}
	}
	msg := fmt.Sprintf("[保留原文] 第 %d 条字幕未翻译：%v", idx+1, errOr(err, "占位标记还原失败"))
	return line.Original, []string{msg}, err
}

func errOr(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// finalize decodes the batch-line newline marker and restores inline markup.
func finalize(raw string, line batchLine) (string, bool) {
	text := decodeBatchLine(strings.TrimSpace(raw))
	restored, ok := RestoreTags(text, line.Tags)
	if !ok {
		return "", false
	}
	if strings.TrimSpace(restored) == "" && strings.TrimSpace(line.Masked) != "" {
		return "", false
	}
	return restored, true
}

var (
	reNumberedPipe    = regexp.MustCompile(`^\s*(\d+)\s*[|｜]\s?(.*)$`)
	reNumberedBracket = regexp.MustCompile(`^\s*\[(\d+)\]\s*(.*)$`)
	reNumberedDot     = regexp.MustCompile(`^\s*(\d+)[.、．]\s*(.*)$`)
)

// parseNumberedResponse maps a model response back onto 1..n source cues.
// It tolerates stray preamble lines, alternative numbering styles, and the
// case where the model drops numbering but keeps exactly n lines.
func parseNumberedResponse(resp string, n int) ([]string, bool) {
	lines := cleanLines(resp)
	if len(lines) == 0 {
		return nil, false
	}
	got := make(map[int]string, n)
	for _, line := range lines {
		var num int
		var text string
		matched := false
		for _, re := range []*regexp.Regexp{reNumberedPipe, reNumberedBracket, reNumberedDot} {
			if m := re.FindStringSubmatch(line); m != nil {
				num, _ = strconv.Atoi(m[1])
				text = m[2]
				matched = true
				break
			}
		}
		if !matched {
			continue // ignore preamble / trailing notes
		}
		if num < 1 || num > n {
			return nil, false // a numbered line outside 1..n means misalignment
		}
		if _, dup := got[num]; dup {
			return nil, false
		}
		got[num] = text
	}
	if len(got) == n {
		out := make([]string, n)
		for i := 0; i < n; i++ {
			v, ok := got[i+1]
			if !ok {
				return nil, false
			}
			out[i] = v
		}
		return out, true
	}
	// Fallback: no numbering at all, but exactly one output line per cue.
	if len(got) == 0 && len(lines) == n {
		return lines, true
	}
	return nil, false
}

func cleanLines(resp string) []string {
	resp = strings.ReplaceAll(resp, "\r\n", "\n")
	var out []string
	for _, line := range strings.Split(resp, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "```") || s == "[DONE]" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// cleanSingle removes wrappers a model sometimes adds around a one-line answer.
func cleanSingle(resp string) string {
	lines := cleanLines(resp)
	if len(lines) == 0 {
		return ""
	}
	s := strings.TrimSpace(strings.Join(lines, "\n"))
	if m := reNumberedPipe.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[2])
	}
	for _, prefix := range []string{"译文：", "译文:", "翻译：", "翻译:", "Translation:", "翻译结果：", "翻译结果:"} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.Trim(s, "\"'`「」“”‘’")
	return strings.TrimSpace(s)
}
