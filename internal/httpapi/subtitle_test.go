package httpapi

import (
	"errors"
	"net/http"
	"testing"

	"strm115/internal/subtitle"
)

// TestRuntimeMinutes 片长解析：TMDB 给 "173 min"，老库里可能是什么都有。
//
// 这条直接决定字幕的时长校验开不开：解析成 0 就等于不校验，
// 奥德赛那种「同名另一部片」的字幕又会混进来。
func TestRuntimeMinutes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"173 min", 173},
		{"173", 173},
		{"2h 53m", 173},
		{"1h", 60},
		{"", 0},
		{"", 0},
		{"unknown", 0},
	} {
		if got := runtimeMinutes(c.in); got != c.want {
			t.Errorf("runtimeMinutes(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestStatusOfSubtitle 字幕失败的状态码：对不上和没有都算 404，其余是后端问题。
func TestStatusOfSubtitle(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{subtitle.ErrNoSubtitle, http.StatusNotFound},
		{errors.New("wrap: " + subtitle.ErrMismatch.Error()), http.StatusBadGateway},
		{errors.New("只有英文 未开启自动翻译"), http.StatusUnprocessableEntity},
		{errors.New("字幕服务返回 502"), http.StatusBadGateway},
	}
	for _, c := range cases {
		if got := statusOfSubtitle(c.err); got != c.want {
			t.Errorf("statusOfSubtitle(%v) = %d，期望 %d", c.err, got, c.want)
		}
	}
	// 真正包了 ErrMismatch 的（%w 包装）才该是 404
	if got := statusOfSubtitle(errWrap(subtitle.ErrMismatch)); got != http.StatusNotFound {
		t.Errorf("包装后的 ErrMismatch 应为 404，实际 %d", got)
	}
}

func errWrap(err error) error {
	return &wrapped{err}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "外层说明: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
