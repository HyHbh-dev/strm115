package subtitle

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func assertCues(t *testing.T, got, want []Cue) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("cue count = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Text != want[i].Text {
			t.Errorf("cue %d text = %q, want %q", i, got[i].Text, want[i].Text)
		}
		if got[i].Start != want[i].Start || got[i].End != want[i].End {
			t.Errorf("cue %d timing = %v..%v, want %v..%v", i, got[i].Start, got[i].End, want[i].Start, want[i].End)
		}
		if got[i].Settings != want[i].Settings {
			t.Errorf("cue %d settings = %q, want %q", i, got[i].Settings, want[i].Settings)
		}
	}
}

// roundTrip parses, re-encodes the original texts, then parses again. The
// timing/text information must survive unchanged.
func roundTrip(t *testing.T, name string, src []byte) (*File, []byte) {
	t.Helper()
	f, err := Parse(name, src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Encode(f, f.Texts())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	f2, err := Parse(name, out)
	if err != nil {
		t.Fatalf("Parse(Encode(...)): %v\noutput:\n%s", err, out)
	}
	assertCues(t, f2.Cues, f.Cues)
	return f, out
}

const srtSample = "1\r\n" +
	"00:00:01,500 --> 00:00:03,000 X1:0 X2:720\r\n" +
	"<i>Hello, world!</i>\r\n" +
	"\r\n" +
	"2\r\n" +
	"00:01:01.250 --> 00:01:04,000\r\n" +
	"First line\r\n" +
	"Second line\r\n" +
	"\r\n"

func TestSRT(t *testing.T) {
	src := append(append([]byte(nil), utf8BOM...), []byte(srtSample)...)
	f, out := roundTrip(t, "movie.srt", src)

	if f.Format != FormatSRT || !f.BOM || f.Newline != "\r\n" {
		t.Errorf("metadata = %+v/%v/%q", f.Format, f.BOM, f.Newline)
	}
	if len(f.Cues) != 2 {
		t.Fatalf("got %d cues", len(f.Cues))
	}
	if f.Cues[0].Start != 1500*time.Millisecond || f.Cues[0].End != 3*time.Second {
		t.Errorf("bad first timing: %v..%v", f.Cues[0].Start, f.Cues[0].End)
	}
	if f.Cues[0].Settings != "X1:0 X2:720" {
		t.Errorf("settings = %q", f.Cues[0].Settings)
	}
	if f.Cues[1].Text != "First line\nSecond line" {
		t.Errorf("multiline text = %q", f.Cues[1].Text)
	}
	if !bytes.HasPrefix(out, utf8BOM) {
		t.Error("BOM lost after encode")
	}
	if !bytes.Contains(out, []byte("00:00:01,500 --> 00:00:03,000")) {
		t.Errorf("SRT timestamp not serialized with comma:\n%s", out)
	}

	// Translating must only replace the text, keeping numbering and timing.
	translated := []string{"<i>你好，世界！</i>", "第一行\n第二行"}
	out2, err := Encode(f, translated)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Parse("movie.srt", out2)
	if err != nil {
		t.Fatal(err)
	}
	if f2.Cues[0].Text != translated[0] || f2.Cues[1].Text != translated[1] {
		t.Errorf("translated text not written back: %+v", f2.Cues)
	}
	if f2.Cues[0].ID != 1 || f2.Cues[1].ID != 2 || f2.Cues[0].Start != f.Cues[0].Start {
		t.Errorf("numbering/timing changed: %+v", f2.Cues)
	}
}

const vttSample = "WEBVTT - Test file\n" +
	"Kind: captions\n" +
	"Language: en\n" +
	"\n" +
	"intro\n" +
	"00:00:01.000 --> 00:00:03.500 align:start position:0%\n" +
	"<v Roger>Hello!</v>\n" +
	"\n" +
	"NOTE this is a comment\n" +
	"keep me\n" +
	"\n" +
	"00:05.100 --> 00:07.900\n" +
	"Short timestamps, with a comma\n"

func TestVTT(t *testing.T) {
	f, out := roundTrip(t, "clip.vtt", []byte(vttSample))
	if f.Format != FormatVTT {
		t.Fatalf("format = %q", f.Format)
	}
	if len(f.Cues) != 2 {
		t.Fatalf("got %d cues: %+v", len(f.Cues), f.Cues)
	}
	if f.Cues[0].Identifier != "intro" {
		t.Errorf("identifier = %q", f.Cues[0].Identifier)
	}
	if f.Cues[0].Settings != "align:start position:0%" {
		t.Errorf("settings = %q", f.Cues[0].Settings)
	}
	if f.Cues[0].Start != time.Second {
		t.Errorf("start = %v", f.Cues[0].Start)
	}
	if f.Cues[1].Start != 5*time.Second+100*time.Millisecond {
		t.Errorf("mm:ss timestamp = %v", f.Cues[1].Start)
	}
	for _, want := range []string{"WEBVTT - Test file", "Language: en", "NOTE this is a comment", "keep me", "align:start"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
}

const assSample = "[Script Info]\n" +
	"Title: Demo\n" +
	"ScriptType: v4.00+\n" +
	"\n" +
	"[V4+ Styles]\n" +
	"Format: Name, Fontname, Fontsize\n" +
	"Style: Default,Arial,20\n" +
	"\n" +
	"[Events]\n" +
	"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
	"Comment: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,do not translate me\n" +
	"Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,{\\i1}Hello, world{\\i0}\\NAgain\n" +
	"Dialogue: 0,0:00:04.00,0:00:06.00,Default,,0,0,0,,Bye\n"

func TestASS(t *testing.T) {
	f, out := roundTrip(t, "ep.ass", []byte(assSample))
	if f.Format != FormatASS {
		t.Fatalf("format = %q", f.Format)
	}
	if len(f.Cues) != 2 {
		t.Fatalf("got %d cues: %+v", len(f.Cues), f.Cues)
	}
	if f.Cues[0].Text != `{\i1}Hello, world{\i0}`+"\n"+`Again` {
		t.Errorf("ASS text = %q", f.Cues[0].Text)
	}
	if !bytes.Equal(out, []byte(assSample)) {
		t.Errorf("ASS skeleton was not preserved byte-for-byte:\n--- got ---\n%s\n--- want ---\n%s", out, assSample)
	}
	if !bytes.Contains(out, []byte("Comment: 0,0:00:01.00")) {
		t.Error("Comment line was lost")
	}

	translated := []string{`{\i1}你好，世界{\i0}` + "\n" + `再来一次`, `再见`}
	out2, err := Encode(f, translated)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out2, []byte(`{\i1}你好，世界{\i0}\N再来一次`)) {
		t.Errorf("ASS newline escape missing:\n%s", out2)
	}
	if !bytes.Contains(out2, []byte("[V4+ Styles]")) {
		t.Error("script sections lost")
	}
}

func TestTXT(t *testing.T) {
	src := []byte("First line\n\nSecond line\n")
	_, out := roundTrip(t, "lines.txt", src)
	if !bytes.Equal(out, src) {
		t.Errorf("TXT skeleton changed: %q vs %q", out, src)
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		data string
		want Format
	}{
		{"a.srt", "1\n00:00:01,000 --> 00:00:02,000\nhi\n", FormatSRT},
		{"a.vtt", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nhi\n", FormatVTT},
		{"a.ass", "[Script Info]\nScriptType: v4.00+\n", FormatASS},
		{"noext", "1\n00:00:01,000 --> 00:00:02,000\nhi\n", FormatSRT},
		{"noext", "WEBVTT\n\nhi\n", FormatVTT},
		{"noext", "[Script Info]\n", FormatASS},
		{"noext", "just some prose\n", FormatTXT},
	}
	for _, c := range cases {
		if got := Detect(c.name, []byte(c.data)); got != c.want {
			t.Errorf("Detect(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"00:00:01,000", time.Second},
		{"00:00:01.5", 1500 * time.Millisecond},
		{"01:02:03,045", time.Hour + 2*time.Minute + 3*time.Second + 45*time.Millisecond},
		{"05:01.1", 5*time.Minute + time.Second + 100*time.Millisecond},
	}
	for _, c := range cases {
		got, err := parseTimestamp(c.in)
		if err != nil {
			t.Errorf("parseTimestamp(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseTimestamp(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if formatSRTTime(time.Hour+2*time.Minute+3*time.Second+45*time.Millisecond) != "01:02:03,045" {
		t.Error("formatSRTTime mismatch")
	}
	if formatVTTTime(1500*time.Millisecond) != "00:00:01.500" {
		t.Error("formatVTTTime mismatch")
	}
}

func TestDefaultOutputPath(t *testing.T) {
	if got := DefaultOutputPath("/tmp/movie.srt", "zh"); got != "/tmp/movie.zh.srt" {
		t.Errorf("got %q", got)
	}
	if got := DefaultOutputPath("clip.vtt", "zh-CN"); got != "clip.zh-CN.vtt" {
		t.Errorf("got %q", got)
	}
}
