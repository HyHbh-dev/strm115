package subtitle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"strm115/internal/netproxy"
)

// DefaultSourceBase 是 OpenSubtitles v3 的 Stremio 插件地址。
//
// 选它而不是 OpenSubtitles 官方 API 的原因：官方要注册 API Key 且有每日配额，
// 这个插件不需要鉴权，还把各种源编码（CP1252/CP1250…）统一转成了 UTF-8 ——
// 省掉一整套编码猜测逻辑。
const DefaultSourceBase = "https://opensubtitles-v3.strem.io"

// Candidate 是一条可下载的字幕。
type Candidate struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Lang     string `json:"lang"`    // eng / zho / fre …
	Name     string `json:"name"`    // 字幕文件原名
	Release  string `json:"release"` // 对应的片源名，挑最匹配的靠它
	Group    string `json:"group"`
	Encoding string `json:"encoding"`
	FPS      int    `json:"fps"`
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
}

// Chinese 判断这个语言码是不是中文（简体/繁体都算）。
//
// OpenSubtitles 回的是 ISO 639-2，而且中文有 zho / chi / zht（繁体）/
// zhs（简体）几种写法。漏掉 zht 的代价很实在：明明有中文字幕，
// 却被当成「没有中文」，转而把英文字幕整片翻译一遍——慢，还费翻译额度。
func (c Candidate) Chinese() bool {
	switch strings.ToLower(c.Lang) {
	case "zh", "zho", "chi", "zht", "zhs", "cn", "chs", "cht",
		"zh-cn", "zh-tw", "zh-hans", "zh-hant":
		return true
	}
	return false
}

// English 判断是不是英文。
func (c Candidate) English() bool {
	switch strings.ToLower(c.Lang) {
	case "eng", "en":
		return true
	}
	return false
}

// 简繁标记。OpenSubtitles 的语言码只到 zho/chi 这一层，简体字幕也常写成
// zho，所以字形只能靠片名里的标记认：字幕组命名习惯是 `zh-CN` / `CHS` /
// `简体`，台港组的则是 `zh-TW` / `CHT` / `繁体`。
var (
	simplifiedMarks = []string{
		"zh-cn", "zh_cn", "zhcn", "zh-hans", "zh_hans", "chs", "zhs",
		"简体", "简中", "简英", "简繁", "gb2312", ".gb",
	}
	traditionalMarks = []string{
		"zh-tw", "zh_tw", "zh-hant", "zh_hant", "cht", "zht",
		"繁体", "繁中", "繁英", "big5",
	}
)

// Script 判断这份字幕的文字形制：+1 简体、-1 繁体、0 认不出。
//
// 两个标记都命中（「简繁英字幕」这类双语版）时算简体：它至少含简体，
// 用户不用对着满屏繁体字看。
func (c Candidate) Script() int {
	s := strings.ToLower(c.Lang + " " + c.Name + " " + c.Release)
	for _, w := range simplifiedMarks {
		if strings.Contains(s, w) {
			return 1
		}
	}
	for _, w := range traditionalMarks {
		if strings.Contains(s, w) {
			return -1
		}
	}
	return 0
}

// Languages 把中文的各种写法归一，方便按语种筛。
const (
	LangChinese = "zh"
	LangEnglish = "en"
)

// Source 是字幕列表接口的客户端。
type Source struct {
	Base string
	HTTP *http.Client
}

func NewSource(base string) *Source {
	if strings.TrimSpace(base) == "" {
		base = DefaultSourceBase
	}
	return &Source{
		Base: strings.TrimRight(base, "/"),
		HTTP: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: netproxy.URL}},
	}
}

// listURL 拼列表地址。电影用 `{type}/{id}.json`，
// 剧集必须带 `:{season}:{episode}`，否则插件不知道该给哪一集。
func (s *Source) listURL(kind, imdbID string, season, episode int) string {
	id := imdbID
	if kind == "series" && season > 0 {
		id = fmt.Sprintf("%s:%d:%d", imdbID, season, episode)
	}
	return fmt.Sprintf("%s/subtitles/%s/%s.json", s.Base, kind, url.PathEscape(id))
}

// wireCandidate 是插件返回的原始字段名。
type wireCandidate struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Lang     string `json:"lang"`
	Name     string `json:"subtitleFileName"`
	Release  string `json:"movieReleaseName"`
	Group    string `json:"releaseGroup"`
	Encoding string `json:"SubEncoding"`
	FPS      int    `json:"fpsMilli"`
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
}

type wireResponse struct {
	Subtitles []wireCandidate `json:"subtitles"`
}

// List 拉字幕列表。没有字幕不算错误，返回空切片。
func (s *Source) List(ctx context.Context, kind, imdbID string, season, episode int) ([]Candidate, error) {
	if !strings.HasPrefix(imdbID, "tt") {
		return nil, fmt.Errorf("不是合法的 IMDb ID: %q", imdbID)
	}
	if kind != "movie" && kind != "series" {
		return nil, fmt.Errorf("不支持的类型: %q", kind)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.listURL(kind, imdbID, season, episode), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "strm115/1.0")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉字幕列表: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("字幕服务返回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var wr wireResponse
	if err := json.Unmarshal(body, &wr); err != nil {
		return nil, fmt.Errorf("解析字幕列表: %w", err)
	}

	out := make([]Candidate, 0, len(wr.Subtitles))
	for _, w := range wr.Subtitles {
		if w.URL == "" {
			continue
		}
		out = append(out, Candidate{
			ID: w.ID, URL: w.URL, Lang: w.Lang, Name: w.Name,
			Release: w.Release, Group: w.Group, Encoding: w.Encoding,
			FPS: w.FPS, Season: w.Season, Episode: w.Episode,
		})
	}
	return out, nil
}

// Download 下载字幕正文。
//
// 返回的字节已经保证是合法 UTF-8：插件在 URL 里带 `subencoding-stremio-utf8`
// 时服务端会转换，但那是"通常"，不是"一定"——转一道很便宜，换来的是
// 后面解析器不会因为一行乱码把整份字幕丢掉。
func (s *Source) Download(ctx context.Context, c Candidate) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "strm115/1.0")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载字幕: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载字幕返回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	if len(body) < 32 {
		return nil, errors.New("字幕内容过短，可能是错误页")
	}
	return toUTF8(body), nil
}

// toUTF8 处理 BOM 与非 UTF-8 字节。
func toUTF8(data []byte) []byte {
	// UTF-8 BOM
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	// UTF-16 BOM：字幕站偶尔会给
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		return utf16ToUTF8(data[2:], true)
	}
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		return utf16ToUTF8(data[2:], false)
	}
	if utf8.Valid(data) {
		return data
	}
	// 不是 UTF-8 也不是 UTF-16：按 Latin-1 处理。
	// 对英文字幕够用（CP1252 与 Latin-1 在字母区完全一致），
	// 中文站点的 GBK 字幕会变成乱码——这种会走到翻译流程，翻译结果照样是中文。
	out := make([]byte, 0, len(data)*2)
	for _, b := range data {
		if b < 0x80 {
			out = append(out, b)
			continue
		}
		out = append(out, 0xC0|b>>6, 0x80|b&0x3F)
	}
	return out
}

func utf16ToUTF8(data []byte, littleEndian bool) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i+1 < len(data); i += 2 {
		var r rune
		if littleEndian {
			r = rune(uint16(data[i]) | uint16(data[i+1])<<8)
		} else {
			r = rune(uint16(data[i])<<8 | uint16(data[i+1]))
		}
		// 代理对：UTF-16 里两个 16 位表示一个字符
		if r >= 0xD800 && r <= 0xDBFF && i+3 < len(data) {
			var lo rune
			if littleEndian {
				lo = rune(uint16(data[i+2]) | uint16(data[i+3])<<8)
			} else {
				lo = rune(uint16(data[i+2])<<8 | uint16(data[i+3]))
			}
			if lo >= 0xDC00 && lo <= 0xDFFF {
				r = (r-0xD800)<<10 | (lo - 0xDC00) + 0x10000
				i += 2
			}
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}

// Ranked 是一条候选，外加它与视频文件名的相似度（0~100）。
type Ranked struct {
	Candidate
	Score int
	// Script 是字形偏好（+1 简体 / -1 繁体 / 0 未知），由 Candidate.Script 预先算好：
	// 放在这里是因为排序比较函数里不能再做字符串扫描。
	Script int
}

// scriptWeight 是简繁偏好在排序里折算的相似度分。
//
// 两条中文候选的片源相似度只差几分时，简体优先。这个值不能太大：
// 片源不同意味着时间轴错位，那比字形是繁体难受得多，所以只有当「片源差」
// 小于这个权重时才让简繁说话。
//
// 现实里这几分的差距比比皆是：「碟中谍8」那条 `zht` 繁体以 1 分之差
// （47 vs 46）赢了 `zh-CN` 简体，于是整片字幕都成了繁体。
const scriptWeight = 15

// Rank 把候选排成「中文在前、英文在后，同语种内简体优先 + 片源名相似度降序」。
//
// 只保留中文和英文：其它语种翻出来还是二手，没必要浪费下载与翻译额度。
//
// 为什么要把整串排好返回，而不是只给一条：相似度只看得见 release 名，
// 而字幕站上同名的另一部片（比如 The Asylum 的「The Odyssey」）会被挂在
// 同一个 IMDb id 下，它的 release 名里多一个前缀反而让 bigram 命中更多、
// 分数更高。所以上层必须能「按顺序多试几条」，再用内容（时长）把冒牌货筛掉。
func Rank(list []Candidate, videoName string) []Ranked {
	var zh, en []Ranked
	for _, c := range list {
		r := Ranked{Candidate: c}
		switch {
		case c.Chinese():
			zh = append(zh, r)
		case c.English():
			en = append(en, r)
		}
	}
	score := func(rs []Ranked) {
		for i := range rs {
			rs[i].Score = similarity(strings.ToLower(rs[i].Release), strings.ToLower(videoName))
			rs[i].Script = rs[i].Candidate.Script()
		}
		sort.SliceStable(rs, func(i, j int) bool {
			ki := rs[i].Score + rs[i].Script*scriptWeight
			kj := rs[j].Score + rs[j].Script*scriptWeight
			if ki != kj {
				return ki > kj
			}
			// 同等相似度时，优先 UTF-8（少一次转码风险）
			return strings.Contains(strings.ToLower(rs[i].Encoding), "utf")
		})
	}
	score(zh)
	score(en)
	return append(zh, en...)
}

// Pick 挑一条最合适的字幕。
//
// 顺序：中文 > 英文；同语种里按「片源名与视频名的相似度」降序。
// 相似度很关键——同名电影有几十个版本，时间轴对不上就是一句一句地错位。
//
// 保留这个签名是给「只要一条」的调用方用的；真正补字幕要走 Rank，
// 因为第一条未必是这部片的（见 Rank 的注释）。
func Pick(list []Candidate, videoName string, preferChinese bool) (Candidate, bool, bool) {
	ranked := Rank(list, videoName)
	if len(ranked) == 0 {
		return Candidate{}, false, false
	}
	return ranked[0].Candidate, ranked[0].Chinese(), true
}

// similarity 用字符 bigram 的重合度算相似度（0~100）。
// 够用且不依赖分词：`Dune.Part.Two.2024.2160p.WEB-DL` 与
// `Dune.Part.Two.2024.REPACK.2160p` 会得到很高的分。
func similarity(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 100
	}
	grams := func(s string) map[string]struct{} {
		r := []rune(s)
		m := make(map[string]struct{}, len(r))
		for i := 0; i+1 < len(r); i++ {
			m[string(r[i:i+2])] = struct{}{}
		}
		if len(r) == 1 {
			m[string(r)] = struct{}{}
		}
		return m
	}
	ga, gb := grams(a), grams(b)
	if len(ga) == 0 || len(gb) == 0 {
		return 0
	}
	hit := 0
	for g := range ga {
		if _, ok := gb[g]; ok {
			hit++
		}
	}
	return hit * 100 / maxInt(len(ga), len(gb))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// 片源等级。字幕的 release 名里往往写着它是从哪种片源扒的，
// 而不同片源的剪辑长短可以差出几分钟 —— 这不是「相似度」能表达的差异。
const (
	TierLow  = 0 // 枪版/偷录/硬字幕：影院偷拍或录屏
	TierMid  = 1 // WEB-DL / WEBRip：流媒体
	TierHigh = 2 // Blu-ray / Remux / BDRip：蓝光
)

// screenCopyWords 是枪版片的标志词。
//
// 只看单词边界：`CAM` 出现在 `CAMRiP` 里要认，但 `CAMERA` 这种正常词
// 不该被误伤（`camera` 在字幕组命名里并不常见，这里也不放进去）。
var screenCopyWords = []string{
	"cam", "hdcam", "camrip", "ts", "hdts", "telesync", "tc", "hdtc",
	"telecine", "scr", "dvdscr", "screener", "hc", "hardsub", "hdcamrip",
}

// hdSourceWords 是高清片源的标志词。
var hdSourceWords = []string{"bluray", "blu-ray", "bdrip", "brrip", "remux", "uhd"}

// webSourceWords 是流媒体片源的标志词。
var webSourceWords = []string{"web", "webrip", "webdl", "web-dl", "hdtv", "hdtvrip"}

// sourceTier 判断一个片源名属于哪一档。认不出来按中间档算：
// 未知不等于枪版，别把没写片源的正常字幕一棍子打死。
func sourceTier(name string) int {
	s := strings.ToLower(name)
	if s == "" {
		return TierMid
	}
	// 先判枪版：`The.Odyssey.2026.1080p.CAM.x264` 里同时有 cam 和 x264，
	// 但没有 web/bluray 词，所以顺序不影响；两边都有的畸形命名以枪版为准。
	for _, w := range screenCopyWords {
		if hasWord(s, w) {
			return TierLow
		}
	}
	for _, w := range hdSourceWords {
		if hasWord(s, w) {
			return TierHigh
		}
	}
	for _, w := range webSourceWords {
		if hasWord(s, w) {
			return TierMid
		}
	}
	return TierMid
}

// hasWord 判断 s 里有没有独立出现的 w（前后不是字母数字）。
func hasWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isAlnum(s[j-1])
		after := j+len(w) >= len(s) || !isAlnum(s[j+len(w)])
		if before && after {
			return true
		}
		i = j + len(w)
	}
}

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// ScreenCopy 判断这条候选是不是枪版片源。
func (r Ranked) ScreenCopy() bool {
	if sourceTier(r.Release) == TierLow {
		return true
	}
	return sourceTier(r.Name) == TierLow
}
