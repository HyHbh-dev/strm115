package subtitle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"strm115/internal/store"
	"strm115/internal/translate"
)

// ErrNoSubtitle 表示这个片源一条字幕都没有。
var ErrNoSubtitle = errors.New("这个片源没有字幕")

// ErrMismatch 表示有字幕，但都跟这部片对不上（时长差太远）。
//
// 和 ErrNoSubtitle 分开是为了让界面能说清楚：一个是「字幕站没有」，
// 一个是「有，但不是我这部片的」——后者用户还可以去别处找，前者只能等。
var ErrMismatch = errors.New("字幕和片源对不上")

const (
	// defaultMaxTry 是每个语种最多试几条候选。
	//
	// 有上限是因为每条都要下载+解析几十 KB，整库补字幕时没有上限会把
	// 公共接口打疼；三条足够覆盖「冒牌片排第一、正主排第二」这种常见排序。
	defaultMaxTry = 3
	// minCover / maxCover 是「字幕覆盖时长 / 片长」的允许区间。
	//
	// 下限 0.75：正片 173 分钟、字幕最后一条对白在 164 分钟（片尾字幕没人
	// 配音）是常态；而上限 1.15 挡住「把整季或加长版字幕塞进来」。
	// 冒牌片那条第 83 分钟就结束，比例 0.48，直接出局。
	minCover = 0.75
	maxCover = 1.15
)

// Options 是一次「补字幕」的运行参数。
type Options struct {
	// Translate 为 true 时，没有可用的中文字幕才把英文字幕翻译成中文。
	Translate bool
	// Bilingual 输出双语（译文在上、原文在下）。
	Bilingual bool
	// Overwrite 覆盖已存在的中文字幕。
	Overwrite bool
	// APIBase / Model 是翻译服务地址与模型。
	APIBase string
	Model   string
	// Workers 翻译并发，默认 2（公共接口，别把人打挂）。
	Workers int
	// SaveSource 额外保留一份英文字幕（便于对照/换翻译）。
	SaveSource bool
	// RuntimeMin 是这条视频的片长（分钟），0 表示不知道。
	//
	// 用来判断一份字幕是不是这部片的：字幕站会把同名但不同的另一部片挂在
	// 同一个 IMDb 下（奥德赛就中过——The Asylum 的山寨片，83 分钟），
	// release 名分不出来，时长能。
	RuntimeMin int
	// MaxTry 每个语种最多试几条候选，0 用 defaultMaxTry。
	MaxTry int
}

// Target 描述要补字幕的那个视频。
type Target struct {
	RelPath   string // 相对媒体库根的 .strm 路径
	VideoName string // 原视频文件名，用来挑最匹配的片源
	IMDbID    string
	Kind      string // movie / series
	Season    int
	Episode   int
}

// Result 是一次补字幕的结果。
type Result struct {
	RelPath    string `json:"rel_path"`
	Saved      string `json:"saved,omitempty"` // 中文字幕的相对路径
	SourceLang string `json:"source_lang"`     // 原始字幕语种
	Origin     string `json:"origin"`          // zh / translated
	Translated bool   `json:"translated"`
	Cues       int    `json:"cues"`
	Note       string `json:"note,omitempty"`
	Skipped    bool   `json:"skipped,omitempty"`
	// Release / Score 是最后选中的那条字幕的片源名与相似度。
	// 排查「字幕错位」时，第一个要看的就是它是不是这部片的版本。
	Release string `json:"release,omitempty"`
	Score   int    `json:"match,omitempty"`
	// Tried 是按顺序试过、但被放弃的候选（含放弃原因）。
	Tried []string `json:"tried,omitempty"`
}

// Fetcher 负责「查字幕 → 下载 → 需要时翻译 → 落盘」。
type Fetcher struct {
	Source *Source
	Store  *store.Store
	Log    *slog.Logger
	// Progress 可选，用来把「正在翻译第几批」这类信息透出去。
	Progress func(msg string)
}

func NewFetcher(st *store.Store, log *slog.Logger) *Fetcher {
	if log == nil {
		log = slog.Default()
	}
	return &Fetcher{Source: NewSource(""), Store: st, Log: log}
}

func (f *Fetcher) progress(msg string) {
	if f.Progress != nil {
		f.Progress(msg)
	}
}

// Fetch 给一个视频补中文字幕。
func (f *Fetcher) Fetch(ctx context.Context, root string, t Target, opt Options) (*Result, error) {
	res := &Result{RelPath: t.RelPath}

	if t.IMDbID == "" {
		return nil, errors.New("没有 IMDb ID，无法查字幕")
	}

	videoPath := filepath.Join(root, filepath.FromSlash(t.RelPath))
	// .strm 的字幕名必须是「去掉 .strm 再加 .zh.srt」：
	// Emby 只认与视频同名（不含扩展名）的字幕，`.strm.zh.srt` 会被当成孤儿文件。
	base := strings.TrimSuffix(videoPath, filepath.Ext(videoPath))
	zhPath := base + ".zh.srt"

	// replacingTrad 记录「这次是不是为了把繁体换成简体才继续的」。
	// 只有它成立时，才允许在候选仍然全是繁体的情况下放弃覆盖、保留原文件。
	var replacingTrad bool
	if !opt.Overwrite {
		if existing := findExistingChinese(base); existing != "" {
			// 已有的是繁体时重下一份简体。
			//
			// 以前这里一律跳过，于是「第一次补到繁体」就等于永久繁体：字幕站
			// 上同一部片常同时挂着 zht 和 zh-CN 两条，用户把繁体的删掉重补，
			// 只要排序没修好就还是同一条。排序修好之后这里补上最后一环 ——
			// 否则整库补字幕永远认为「已经有中文字幕」，一辈子不会换。
			//
			// 只认自己写的 .zh.srt：`.chs.srt` 这些很可能是用户手放的，
			// 那种情况仍然一个字节都不动。
			if existing == zhPath && fileLooksTraditional(existing) {
				opt.Overwrite = true
				replacingTrad = true
				res.Note = "已有字幕是繁体，重下一份简体"
			} else {
				res.Saved = relTo(root, existing)
				res.Origin = "existing"
				res.Skipped = true
				res.Note = "已经有中文字幕"
				return res, nil
			}
		}
	}

	if opt.Workers <= 0 {
		opt.Workers = 2
	}

	f.progress("查询字幕列表")
	list, err := f.Source.List(ctx, t.Kind, t.IMDbID, t.Season, t.Episode)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNoSubtitle
	}
	// 插件并不保证只回这一集：实测查 S01E199 时回来的是同一部剧的
	// `Special II Ep 99` / `Ep 93`。片名相似度 + 片长能挡住大部分，但
	// 「别的集的字幕恰好时长接近」是挡不住的，先用季集号硬筛一遍。
	// 一条都不剩时按「对不上」而不是「没有字幕」报，用户才知道该去别处找。
	list, dropped := filterByEpisode(list, t)
	if len(list) == 0 && dropped > 0 {
		return nil, fmt.Errorf("%w：返回的 %d 条字幕都不是 S%02dE%02d",
			ErrMismatch, dropped, t.Season, t.Episode)
	}
	if len(list) == 0 {
		return nil, ErrNoSubtitle
	}

	ranked := Rank(list, t.VideoName)

	// 片源分级：视频是蓝光/Remux 时，枪版（CAM / TS / 硬字幕）字幕直接不要。
	//
	// 理由不是「画质」，是「剪辑」：同一部片的枪版和蓝光能差出几分钟，
	// 时间轴是整段平移的，不是抖一抖 —— 长度校验（fitsRuntime）看总时长，
	// 这种偏移天生看不见。宁可用英文字幕 AI 翻一遍，也不给用户一份
	// 说一句话差一分钟的字幕。
	skippedScreen := 0
	if sourceTier(t.VideoName) >= TierHigh {
		keep := func(rs []Ranked) []Ranked {
			out := rs[:0]
			for _, r := range rs {
				if r.ScreenCopy() {
					skippedScreen++
					res.Tried = append(res.Tried, releaseOf(r)+"（枪版字幕，片源是蓝光）")
					continue
				}
				out = append(out, r)
			}
			return out
		}
		ranked = append(keep(ranked[:countChinese(ranked)]), keep(ranked[countChinese(ranked):])...)
	}

	var zh, en []Ranked
	for _, r := range ranked {
		if r.Chinese() {
			zh = append(zh, r)
		} else {
			en = append(en, r)
		}
	}
	if len(zh)+len(en) == 0 {
		if skippedScreen > 0 {
			return nil, fmt.Errorf("%w：只有 %d 条枪版字幕，片源是蓝光，已跳过（%s）",
				ErrMismatch, skippedScreen, strings.Join(res.Tried, "；"))
		}
		return nil, ErrNoSubtitle
	}

	// —— 第一优先：下载中文字幕 ——
	// 只按语种挑一条是不够的：同一个 IMDb 下可能挂着同名另一部片的字幕，
	// 而且它常常因为 release 名里多个前缀而分数更高。所以这里按相似度
	// 顺序多试几条，用「字幕覆盖时长 vs 片长」把冒牌货筛掉。
	var (
		cand *Ranked
		file *File
		raw  []byte
	)
	if len(zh) > 0 {
		cand, file, raw, err = f.tryCandidates(ctx, zh, opt, res)
		if err != nil && cand == nil {
			// 全部下载/解析失败：这不是「没有字幕」，但也没法继续翻译
			// （英文字幕还没试），留到最后再判。
			f.Log.Warn("中文字幕候选全部下载失败", "rel", t.RelPath, "err", err)
		}
	}

	if cand == nil {
		// 走到这里说明：要么一条中文都没有，要么几条中文都跟这部片对不上。
		if len(zh) > 0 && !opt.Translate {
			return nil, fmt.Errorf("%w：%d 条中文字幕的时长都和片长 %d 分钟差太远（%s）",
				ErrMismatch, len(zh), opt.RuntimeMin, strings.Join(res.Tried, "；"))
		}
		if len(en) == 0 {
			if len(zh) > 0 {
				return nil, fmt.Errorf("%w：%s", ErrMismatch, strings.Join(res.Tried, "；"))
			}
			return nil, ErrNoSubtitle
		}
		if !opt.Translate {
			return nil, fmt.Errorf("只有%s字幕，未开启自动翻译", langLabel(en[0].Lang))
		}
		// —— 第二优先：英文字幕 + AI 翻译 ——
		cand, file, raw, err = f.tryCandidates(ctx, en, opt, res)
		if err != nil || cand == nil {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w：英文字幕也都对不上片长 %d 分钟", ErrMismatch, opt.RuntimeMin)
		}
	}

	res.Cues = len(file.Cues)
	res.SourceLang = cand.Lang
	res.Release = cand.Release
	res.Score = cand.Score

	if cand.Chinese() {
		if replacingTrad && cand.Script < 0 {
			// 转了一圈还是繁体：这部片在字幕站根本没有简体源。
			// 别再覆盖一遍（下次整库补字幕又会重下，纯属白花请求）。
			res.Saved = relTo(root, zhPath)
			res.Origin = "existing"
			res.Skipped = true
			res.Note = "字幕站只有繁体版，保留原字幕"
			return res, nil
		}
		if err := writeAtomic(zhPath, raw); err != nil {
			return nil, err
		}
		finalizeSubtitle(base, videoPath, opt.Overwrite, f.Log)
		res.Saved = relTo(root, zhPath)
		res.Origin = "zh"
		return res, nil
	}

	// —— 没有可用的中文：翻译 ——
	if len(file.Cues) == 0 {
		return nil, errors.New("字幕里没有可翻译的台词")
	}
	f.progress(fmt.Sprintf("翻译 %d 条字幕", len(file.Cues)))
	translated, trNote, err := f.translate(ctx, file.Texts(), opt)
	if err != nil {
		return nil, err
	}
	if opt.Bilingual {
		origin := file.Texts()
		for i := range translated {
			if strings.TrimSpace(translated[i]) == "" || translated[i] == origin[i] {
				continue
			}
			translated[i] = translated[i] + "\n" + origin[i]
		}
	}

	out, err := Encode(file, translated)
	if err != nil {
		return nil, fmt.Errorf("重建字幕失败: %w", err)
	}
	if err := writeAtomic(zhPath, out); err != nil {
		return nil, err
	}
	finalizeSubtitle(base, videoPath, opt.Overwrite, f.Log)
	res.Saved = relTo(root, zhPath)
	res.Origin = "translated"
	res.Translated = true
	res.Note = trNote

	if opt.SaveSource {
		enPath := base + "." + langCode(cand.Lang) + ".srt"
		if _, err := os.Stat(enPath); err != nil || opt.Overwrite {
			if err := writeAtomic(enPath, raw); err != nil {
				f.Log.Warn("保存原文字幕失败", "path", enPath, "err", err)
			}
		}
	}
	return res, nil
}

// tryCandidates 按相似度顺序下载候选，返回第一份「像这部片的」字幕。
//
// 放弃一条候选的两种情况都记进 res.Tried，界面上能直接看到「试过哪几条、
// 为什么不要」——排查字幕错位时，这比一句「字幕失败」有用得多。
func (f *Fetcher) tryCandidates(ctx context.Context, rs []Ranked, opt Options,
	res *Result) (*Ranked, *File, []byte, error) {

	limit := opt.MaxTry
	if limit <= 0 {
		limit = defaultMaxTry
	}
	if limit > len(rs) {
		limit = len(rs)
	}

	var firstErr error
	for i := 0; i < limit; i++ {
		r := rs[i]
		f.progress("下载字幕：" + r.Name)
		raw, err := f.Source.Download(ctx, r.Candidate)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			res.Tried = append(res.Tried, fmt.Sprintf("%s（下载失败）", releaseOf(r)))
			continue
		}
		file, err := Parse("sub.srt", raw)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			res.Tried = append(res.Tried, fmt.Sprintf("%s（解析失败）", releaseOf(r)))
			continue
		}
		if !fitsRuntime(file, opt.RuntimeMin) {
			res.Tried = append(res.Tried, fmt.Sprintf("%s（只有 %d 分钟，片长 %d 分钟）",
				releaseOf(r), roundMinute(file.LastEnd()), opt.RuntimeMin))
			f.Log.Info("跳过时长对不上的字幕",
				"release", r.Release, "cover_min", roundMinute(file.LastEnd()),
				"runtime_min", opt.RuntimeMin)
			continue
		}
		return &r, file, raw, nil
	}
	return nil, nil, nil, firstErr
}

// filterByEpisode 按季集号筛掉「别的集」的字幕。
//
// 候选没带季/集号（0）时一律保留：插件对电影和部分老资源不填这两个字段，
// 拿 0 当「不是这一集」会把这些片子全部误杀。
func filterByEpisode(list []Candidate, t Target) ([]Candidate, int) {
	if t.Episode <= 0 {
		return list, 0
	}
	out := make([]Candidate, 0, len(list))
	dropped := 0
	for _, c := range list {
		if c.Season > 0 && t.Season > 0 && c.Season != t.Season {
			dropped++
			continue
		}
		if c.Episode > 0 && c.Episode != t.Episode {
			dropped++
			continue
		}
		out = append(out, c)
	}
	return out, dropped
}

// fitsRuntime 判断这份字幕是不是这部片的。
//
// 片长未知（比如剧集元数据没给 runtime）时一律放行：宁可错用也不要因为
// 少了元数据就一集都补不上。
func fitsRuntime(file *File, runtimeMin int) bool {
	if runtimeMin <= 0 {
		return true
	}
	end := file.LastEnd()
	if end <= 0 {
		return false
	}
	lo := time.Duration(float64(runtimeMin) * minCover * float64(time.Minute))
	hi := time.Duration(float64(runtimeMin) * maxCover * float64(time.Minute))
	return end >= lo && end <= hi
}

func releaseOf(r Ranked) string {
	if strings.TrimSpace(r.Release) != "" {
		return r.Release
	}
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return r.ID
}

func roundMinute(d time.Duration) int {
	return int(d.Round(time.Minute) / time.Minute)
}

// translate 翻译全部台词，命中数据库缓存的不再调接口。
//
// 缓存是必需的而不是优化：一份两小时的电影有 1000+ 条台词，翻译要几十次请求；
// 用户重刮一遍或者字幕换了个版本，重复请求既慢又浪费公共接口的额度。
func (f *Fetcher) translate(ctx context.Context, texts []string, opt Options) ([]string, string, error) {
	fp := fingerprint(opt.APIBase, opt.Model, "zh")
	keys := make([]string, len(texts))
	for i, t := range texts {
		keys[i] = cacheKey(fp, t)
	}

	cached := map[string]string{}
	if f.Store != nil {
		got, err := f.Store.GetTranslations(ctx, keys)
		if err != nil {
			f.Log.Warn("读翻译缓存失败，按全部未命中处理", "err", err)
		} else {
			cached = got
		}
	}

	out := make([]string, len(texts))
	var missing []int
	for i, t := range texts {
		if v, ok := cached[keys[i]]; ok && strings.TrimSpace(v) != "" {
			out[i] = v
			continue
		}
		if strings.TrimSpace(t) == "" {
			out[i] = t
			continue
		}
		missing = append(missing, i)
	}

	if len(missing) == 0 {
		return out, "全部命中缓存", nil
	}

	client := translate.NewClient(opt.APIBase, opt.Model, 120*time.Second)
	tr := &translate.Translator{
		Client:  client,
		Target:  "zh",
		Workers: opt.Workers,
		Retries: 3,
	}
	tr.OnProgress = func(done, total int) {
		f.progress(fmt.Sprintf("翻译进度 %d/%d 批", done, total))
	}

	batch := make([]string, len(missing))
	for i, idx := range missing {
		batch[i] = texts[idx]
	}
	got, rep, err := tr.Translate(ctx, batch)
	if err != nil {
		return nil, "", fmt.Errorf("翻译失败: %w", err)
	}

	fresh := make(map[string]string, len(missing))
	for i, idx := range missing {
		if i < len(got) {
			out[idx] = got[i]
			// 与原文完全相同的多半是翻译失败的兜底，不写缓存，下次还会重试
			if got[i] != texts[idx] && strings.TrimSpace(got[i]) != "" {
				fresh[keys[idx]] = got[i]
			}
		}
	}
	if f.Store != nil && len(fresh) > 0 {
		if err := f.Store.PutTranslations(ctx, fresh); err != nil {
			f.Log.Warn("写翻译缓存失败", "err", err)
		}
	}

	note := fmt.Sprintf("翻译 %d 条，命中缓存 %d 条", len(missing)-rep.Fallback, len(texts)-len(missing))
	if rep.Fallback > 0 {
		note += fmt.Sprintf("，%d 条保留原文", rep.Fallback)
	}
	return out, note, nil
}

// findExistingChinese 找已经存在的中文字幕文件。
//
// Emby 认的后缀有很多写法，用户也可能是自己放进去的（比如 `.chs.srt`）。
// 只要有一个，就不再下载——覆盖用户手放的字幕是很讨厌的行为。
func findExistingChinese(base string) string {
	for _, tag := range chineseTags {
		for _, ext := range subtitleExts {
			p := base + tag + ext
			if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
				return p
			}
		}
	}
	return ""
}

// traditionalRunes 是一批「简体里根本不会出现」的高频繁体字。
//
// 只挑简繁差异明确的字（對/对、這/这、說/说…），不用一简对多繁的那些
// （干/乾/幹 之类）——那种两边都可能出现，拿来判断只会误伤。
var traditionalRunes = func() map[rune]struct{} {
	const s = "們這個說時會來對開關麼樣為與學國還進實點電話車門問間見現頭應該讓覺過發東幾從聽寫買賣錢鄉親愛戰軍無萬與專業義樂習書處給帶處"
	m := make(map[rune]struct{}, len([]rune(s)))
	for _, r := range s {
		m[r] = struct{}{}
	}
	return m
}()

// looksTraditional 判断一段字幕正文是不是繁体。
//
// 阈值给到 5：引用、人名里偶尔蹦出一个繁体字不算数；一整份字幕里这些字
// 出现 5 次以上，基本只可能是繁体版。
func looksTraditional(data []byte) bool {
	hits := 0
	for _, r := range string(data) {
		if _, ok := traditionalRunes[r]; ok {
			hits++
			if hits >= 5 {
				return true
			}
		}
	}
	return false
}

// fileLooksTraditional 读字幕文件判断是不是繁体。读不出来就当不是 ——
// 保守的一侧是不动用户文件。
func fileLooksTraditional(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	// 只读前 256KB：正文开头几十行就够判断了，大文件也不必整份读进来。
	data, err := io.ReadAll(io.LimitReader(f, 256<<10))
	if err != nil {
		return false
	}
	return looksTraditional(data)
}

// chineseTags / subtitleExts 是 findExistingChinese 认的那批后缀，
// 覆盖重下时也要用同一份清单去清理，别两处越写越不一致。
var (
	chineseTags  = []string{".zh", ".chs", ".cht", ".zh-cn", ".zh-hans", ".zh-tw", ".chi", ".zho"}
	subtitleExts = []string{".srt", ".ass", ".ssa", ".vtt"}
)

// finalizeSubtitle 在「覆盖式重新下载」成功后收尾。平时不覆盖时一律不动。
//
// 两件事都是为了让 Emby 真的换字幕，而不是继续用它第一次扫描时登记的那份：
//  1. 同目录里其余语言标签的中文字幕（.chs/.cht/...）改名为 .old。
//     Emby 的 MediaStreams 里记的是文件路径，新写一份 .zh.srt 并不会让它
//     忘掉旧的那份，结果就是「文件明明换了、播放器里还是旧字幕」。
//  2. bump 一下 .strm 的 mtime。Emby 判断「这个条目有没有变」看的是主媒体
//     文件；只改旁边的 .srt 时，条目刷新可能整段跳过 ffprobe。
//     改的是时间戳不是内容，播放直链不受影响。
//
// 不删除旧文件：改个扩展名即可，用户想找回随时能找回来。
func finalizeSubtitle(base, videoPath string, overwrite bool, log *slog.Logger) {
	if !overwrite {
		return
	}
	keep := base + ".zh.srt"
	for _, tag := range chineseTags {
		for _, ext := range subtitleExts {
			p := base + tag + ext
			if p == keep {
				continue
			}
			if st, err := os.Stat(p); err != nil || st.IsDir() {
				continue
			}
			bak := p + ".old"
			_ = os.Remove(bak) // 上一轮留下的备份，覆盖掉即可
			if err := os.Rename(p, bak); err != nil && log != nil {
				log.Warn("旧字幕改名失败", "path", p, "err", err)
			}
		}
	}
	if _, err := os.Stat(videoPath); err == nil {
		now := time.Now()
		if err := os.Chtimes(videoPath, now, now); err != nil && log != nil {
			log.Warn("刷新主媒体文件时间戳失败", "path", videoPath, "err", err)
		}
	}
}

// fingerprint 标识翻译配置。配置一变，缓存自动失效。
func fingerprint(apiBase, model, target string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{apiBase, model, target}, "\x00")))
	return hex.EncodeToString(h[:])[:16]
}

func cacheKey(fp, text string) string {
	h := sha256.Sum256([]byte(fp + "\x00" + text))
	return hex.EncodeToString(h[:])
}

func langLabel(code string) string {
	switch strings.ToLower(code) {
	case "eng", "en":
		return "英文"
	case "fre", "fra":
		return "法文"
	case "ger", "deu":
		return "德文"
	case "spa":
		return "西班牙文"
	case "jpn":
		return "日文"
	case "kor":
		return "韩文"
	case "rus":
		return "俄文"
	}
	return code + " 语"
}

// langCode 把插件用的三字母码转成字幕文件后缀常用的两字母码。
func langCode(code string) string {
	switch strings.ToLower(code) {
	case "eng":
		return "en"
	case "fre", "fra":
		return "fr"
	case "ger", "deu":
		return "de"
	case "spa":
		return "es"
	case "jpn":
		return "ja"
	case "kor":
		return "ko"
	case "rus":
		return "ru"
	}
	return strings.ToLower(code)
}

func writeAtomic(dest string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func relTo(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// SubtitlePathFor 推算某个视频对应的中文字幕落盘路径（供上层展示用）。
func SubtitlePathFor(relPath string) string {
	ext := path.Ext(relPath)
	return strings.TrimSuffix(relPath, ext) + ".zh.srt"
}

// countChinese 数 Rank 结果里中文打头的候选有几条。
// Rank 保证「中文全部在前」，所以这个数就是中文段的长度。
func countChinese(rs []Ranked) int {
	n := 0
	for _, r := range rs {
		if r.Chinese() {
			n++
		}
	}
	return n
}
