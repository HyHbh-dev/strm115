package httpapi

// 入库通知与刮削完成通知。
//
// 为什么挂在刮削之后而不是同步之后：同步跑完时只有文件名，没有标题、评分、
// 海报 —— 那时候推出去只能是一串 .strm 名字。刮削结束才是「这部片长什么样
// 已经知道了」的时刻，也只有这时才有海报可以附带。
//
// 为什么要 diff 台账：刮削是整库跑的，直接按「本轮跑过的条目」推会把老片
// 重新推一遍。开跑前记一份 rel_path 快照，跑完只认「之前没有的剧根」，
// 才等价于「这次新入库的片」。

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"strm115/internal/notify"
	"strm115/internal/scrape"
	"strm115/internal/store"
)

// ingestNotifyLimit 一次最多推几条带图入库卡片。
//
// 整库首次刮削会一次冒出几十部片，全推等于刷屏。超出的合并成一条文字，
// 用户真想看细节会去海报墙，而不是在聊天窗口里翻三十张图。
const ingestNotifyLimit = 3

// startScrape 起一轮刮削，同时记下开跑前的台账快照。
//
// 所有刮削入口都该走这里：少写一处，那一轮就不会有「新片入库」通知，
// 而且不会有任何报错提示。
func (s *Server) startScrape(lib store.Library, opt scrape.Options) (*scrape.Job, error) {
	before, err := s.st.MediaRelPaths(context.Background(), lib.ID)
	if err != nil {
		s.log.Debug("刮削前读台账失败，本轮不推入库通知", "lib", lib.Name, "err", err)
		before = nil
	}
	job, err := s.scrape.Start(lib, opt)
	if err != nil {
		return nil, err
	}
	s.notifyScrapeDone(lib, job, before)
	return job, nil
}

// notifyScrapeDone 等这一轮跑完，推「刮削完成」+「新片入库」。
func (s *Server) notifyScrapeDone(lib store.Library, job *scrape.Job, before map[string]struct{}) {
	go func() {
		<-job.Done()
		p := job.Snapshot()
		if p.Phase != "done" {
			return
		}
		s.pushEvent("scrape_ok", notify.KindIngest, "🏷 刮削完成 · "+lib.Name,
			fmt.Sprintf("命中 %d · 未匹配 %d · 失败 %d", p.Matched, p.Unmatched, p.Failed))
		// before 为 nil 说明开跑前没读到台账，diff 出来的会是一整库 —— 宁可这次不推。
		if before == nil || !s.eventOn("ingest") {
			return
		}
		s.pushNewIngests(lib, before)
	}()
}

// pushNewIngests 把「这一轮才出现的剧根」逐条推出去。
func (s *Server) pushNewIngests(lib store.Library, before map[string]struct{}) {
	ctx := context.Background()
	rows, err := s.st.ListMediaAll(ctx, lib.ID)
	if err != nil || len(rows) == 0 {
		return
	}
	root := s.libRoot(lib)
	sizes := s.fileSizes(ctx, lib.ID)

	type group struct {
		key  string
		rows []store.Media
	}
	groups := map[string]*group{}
	order := make([]string, 0, 8)
	for _, m := range rows {
		// 没刮出元数据的不算入库：它在墙上还是一条「未匹配」，推出去只会让人疑惑。
		if m.Status != "ok" || strings.TrimSpace(m.Title) == "" {
			continue
		}
		key := scrape.ShowRoot(m.RelPath)
		if key == "" {
			key = m.RelPath
		}
		g := groups[key]
		if g == nil {
			g = &group{key: key}
			groups[key] = g
			order = append(order, key)
		}
		g.rows = append(g.rows, m)
	}

	fresh := make([]*group, 0, ingestNotifyLimit+1)
	for _, key := range order {
		g := groups[key]
		if !isNewGroup(g.rows, before) {
			continue
		}
		fresh = append(fresh, g)
	}
	if len(fresh) == 0 {
		return
	}
	sort.SliceStable(fresh, func(i, j int) bool {
		return tastyFirst(fresh[i].rows[0]) && !tastyFirst(fresh[j].rows[0])
	})

	for i, g := range fresh {
		if i >= ingestNotifyLimit {
			break
		}
		title, body, poster := ingestText(root, g.rows, sizes)
		s.pushEventImage("ingest", notify.KindIngest, title, body, poster)
	}
	if rest := len(fresh) - ingestNotifyLimit; rest > 0 {
		s.pushEvent("ingest", notify.KindIngest, "📥 另有 "+fmt.Sprint(rest)+" 部已入库",
			"去看海报墙：影视库 → 海报墙")
	}
}

// isNewGroup 判这一组是不是本轮新出现的。
//
// 只要组里有一条 rel_path 不在开跑前的快照里就算新 —— 剧集新追一季、
// 电影换了版本都会走到这里，而重刮老片（overwrite）不会多出 rel_path。
func isNewGroup(rows []store.Media, before map[string]struct{}) bool {
	for _, m := range rows {
		if _, ok := before[m.RelPath]; !ok {
			return true
		}
	}
	return false
}

// tastyFirst 让电影排在剧集前面：一次入库里电影通常是用户刚下完想看的，
// 剧集可能一整季、推一张卡就够。
func tastyFirst(m store.Media) bool { return m.Kind != "series" }

// ingestText 组装一条入库消息：标题、类型、评分、分辨率、集数、文件数与体积。
func ingestText(root string, rows []store.Media, sizes map[string]int64) (title, body, poster string) {
	rep := rows[0]
	for _, m := range rows {
		if m.Episode > 0 && (rep.Episode == 0 || m.Episode < rep.Episode) {
			rep = m
		}
	}

	name := strings.TrimSpace(rep.Title)
	if name == "" {
		name = strings.TrimSpace(rep.MetaTitle)
	}
	if name == "" {
		name = path.Base(rep.RelPath)
	}
	title = "🆕 " + name
	if y := strings.TrimSpace(rep.Year); y != "" {
		title += "（" + y + "）"
	}

	head := "🎬 电影"
	if rep.Kind == "series" {
		head = "📺 剧集"
	}
	if g := firstGenre(rep.Genres); g != "" {
		head += " · " + g
	}
	if r := strings.TrimSpace(rep.Rating); r != "" {
		head += " · ⭐ " + r
	}

	lines := []string{head}
	if q := qualityOf(rows); q != "" {
		lines = append(lines, "📀 "+q)
	}
	lines = append(lines, "📦 "+volumeOf(rows, sizes))
	body = strings.Join(lines, "\n")

	poster = posterPath(root, rep, rows)
	return title, body, poster
}

// firstGenre 取第一个类型：一排「动作 / 科幻 / 冒险」在手机上会挤成两行。
func firstGenre(s string) string {
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			return p
		}
	}
	return ""
}

var (
	resPattern   = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4k)\b`)
	hdrPattern   = regexp.MustCompile(`(?i)\b(dv|dolby\s*vision|hdr10\+|hdr10|hdr)\b`)
	remuxPattern = regexp.MustCompile(`(?i)\bremux\b`)
	webPattern   = regexp.MustCompile(`(?i)\b(web-?dl|webrip|bluray|blu-ray|bdrip)\b`)
)

// qualityOf 从文件名里捞「1080P HDR WEB-DL」这类标记。
//
// 不解析就不显示，绝不猜：把 720p 的片写成 1080P 比不写更糟。
func qualityOf(rows []store.Media) string {
	src := rows[0].RelPath
	parts := make([]string, 0, 3)
	if m := resPattern.FindString(src); m != "" {
		parts = append(parts, strings.ToUpper(m))
	}
	if m := hdrPattern.FindString(src); m != "" {
		parts = append(parts, strings.ToUpper(strings.ReplaceAll(m, " ", "")))
	}
	if remuxPattern.MatchString(src) {
		parts = append(parts, "REMUX")
	} else if m := webPattern.FindString(src); m != "" {
		parts = append(parts, strings.ToUpper(m))
	}
	return strings.Join(parts, " ")
}

// volumeOf 生成「S01E01-E10 · 10 个文件 · 8.4 GB」。
func volumeOf(rows []store.Media, sizes map[string]int64) string {
	var total int64
	for _, m := range rows {
		total += sizes[m.RelPath]
	}
	parts := make([]string, 0, 3)
	if span := episodeSpan(rows); span != "" {
		parts = append(parts, span)
	}
	files := len(rows)
	if files == 1 {
		parts = append(parts, "1 个文件")
	} else {
		parts = append(parts, fmt.Sprintf("%d 个文件", files))
	}
	if total > 0 {
		parts = append(parts, humanSize(total))
	}
	return strings.Join(parts, " · ")
}

// episodeSpan 生成 S01E01-E10 / S01E05 / 「10 集」这类集数区间。
//
// 跨季时说不出「S01-S02 第几集」这种话（每季集数不同），直接报总集数；
// 没有季号（日番写 C01 的那种）同样报集数 —— 写错格式比不写更糟。
func episodeSpan(rows []store.Media) string {
	lo, hi, season, count := 0, 0, 0, 0
	for _, m := range rows {
		if m.Episode <= 0 {
			// 一条没有集号就整组退回「N 集」，否则算出来的区间是假的
			count = 0
			break
		}
		count++
		if lo == 0 || m.Episode < lo {
			lo = m.Episode
		}
		if m.Episode > hi {
			hi = m.Episode
		}
		if season == 0 {
			season = m.Season
		} else if m.Season != season {
			season = -1 // 跨季
		}
	}
	if count == 0 || lo == 0 {
		return ""
	}
	if season <= 0 {
		return fmt.Sprintf("%d 集", count)
	}
	if lo == hi {
		return fmt.Sprintf("S%02dE%02d", season, lo)
	}
	return fmt.Sprintf("S%02dE%02d-E%02d", season, lo, hi)
}

// posterPath 返回海报的本地绝对路径，供 Telegram 直传。
//
// 剧集的图在剧根、落库的是单集行，所以按剧根兜底找一遍同名文件。
// 找不到就返回空串 —— 渠道会退回纯文本，而不是拿一个必然失败的路径去试。
func posterPath(root string, rep store.Media, rows []store.Media) string {
	if root == "" {
		return ""
	}
	cands := make([]string, 0, 3)
	if p := strings.TrimSpace(rep.PosterFile); p != "" {
		cands = append(cands, p)
	}
	for _, m := range rows {
		if p := strings.TrimSpace(m.PosterFile); p != "" {
			cands = append(cands, p)
		}
	}
	show := scrape.ShowRoot(rep.RelPath)
	if show != "" {
		cands = append(cands, path.Join(show, "poster.jpg"), path.Join(show, "folder.jpg"))
	}
	for _, rel := range cands {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if st, err := os.Stat(abs); err == nil && st.Size() > 0 {
			return abs
		}
	}
	return ""
}

// fileSizes 读台账里每个文件的体积（rel_path → bytes）。
func (s *Server) fileSizes(ctx context.Context, libraryID int64) map[string]int64 {
	files, err := s.st.ListFilesAll(ctx, libraryID)
	if err != nil {
		return nil
	}
	out := make(map[string]int64, len(files))
	for _, f := range files {
		out[f.RelPath] = f.Size
	}
	return out
}
