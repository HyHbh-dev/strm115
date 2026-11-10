package embygw

// 简体中文字幕优先。
//
// 起因：一部 MULTI REMUX 片源里往往自带一整包内嵌字幕，其中中文那条常常是
// 「BTM TRAD」（繁体）。本站补字幕生成的 `.zh.srt` 是简体，但 PlaybackInfo 里
// 两条中文轨都 IsDefault=false、DefaultSubtitleStreamIndex 缺失，客户端按
// 「Chinese」自动匹配时先撞上内嵌繁体轨 —— 用户看到的就是繁体，还会以为
// 补字幕没生效。
//
// 更隐蔽的是外挂字幕取不到：Emby 交付外挂字幕的正确路径必须带 MediaSourceId
// 这一段（/emby/Videos/{id}/{sourceID}/Subtitles/{idx}/Stream.srt），而不少
// 第三方播放器按简化路径（/emby/Videos/{id}/Subtitles/{idx}/Stream.srt）
// 请求，Emby 直接 404。于是播放器只能从 MKV 里读内嵌轨，简体字幕彻底落空。
//
// 这里做两件事：改写 PlaybackInfo 时把本站简体轨设成默认并给出正确的
// DeliveryUrl；ServeHTTP 里把简化路径补上 sourceID 段再反代。

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// reSimpleSubtitle 匹配「缺 MediaSourceId 段」的简化字幕路径。
// 完整路径 /emby/Videos/515/mediasource_515/Subtitles/36/Stream.srt 不会命中
// （中间多了一段），原样反代给 Emby 即可。
var reSimpleSubtitle = regexp.MustCompile(
	`(?i)^/(?:emby/)?(?:videos|items)/([^/]+)/Subtitles/([^/]+)/Stream\.[A-Za-z0-9]+$`)

// 本站补字幕写出来的文件名后缀（见 internal/subtitle/fetch.go 的 chineseTags）。
// 只认这些：别家外挂字幕、用户随手放的其它语言字幕都不该被强制设成默认。
var simplifiedSuffixes = []string{
	".zh.srt", ".chs.srt", ".zh-cn.srt", ".zh_cn.srt", ".zh-hans.srt", ".gb.srt",
}

// 片源内嵌繁体轨的常见标记。
var traditionalMarks = []string{
	"trad", "cht", "zht", "big5", "繁體", "繁体", "正體", "正体", "zh-tw", "zh-hant",
}

func isChineseLang(v any) bool {
	s, _ := v.(string)
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "zh", "zho", "chi", "zht", "zhs", "cn", "chs", "cht",
		"zh-cn", "zh-tw", "zh-hans", "zh-hant":
		return true
	}
	return false
}

func looksTraditionalTitle(title, lang string) bool {
	s := strings.ToLower(title + " " + lang)
	for _, m := range traditionalMarks {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isOurSimplifiedSubtitle 判断一条字幕流是不是本站生成的简体外挂字幕。
func isOurSimplifiedSubtitle(st map[string]any) bool {
	if typ, _ := st["Type"].(string); !strings.EqualFold(typ, "Subtitle") {
		return false
	}
	if ext, _ := st["IsExternal"].(bool); !ext {
		return false
	}
	if !isChineseLang(st["Language"]) {
		return false
	}
	p, _ := st["Path"].(string)
	lp := strings.ToLower(strings.TrimSpace(p))
	for _, suf := range simplifiedSuffixes {
		if strings.HasSuffix(lp, suf) {
			return true
		}
	}
	return false
}

func streamIndex(st map[string]any) (int, bool) {
	switch v := st["Index"].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// subtitleDeliveryURL 拼 Emby 认可的完整外挂字幕地址（必须带 MediaSourceId 段）。
// 用相对路径：客户端沿用自己正在访问的协议 / 域名 / 反代前缀。
func subtitleDeliveryURL(itemID, sourceID, token, userID string, index int) string {
	if sourceID == "" {
		return ""
	}
	q := url.Values{}
	if token != "" {
		q.Set("api_key", token)
		q.Set("ApiKey", token)
	}
	if userID != "" {
		q.Set("UserId", userID)
	}
	u := "/emby/Videos/" + url.PathEscape(itemID) + "/" +
		url.PathEscape(sourceID) + "/Subtitles/" + strconv.Itoa(index) +
		"/Stream.srt"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// preferSimplifiedSubtitles 改写单个 MediaSource 的字幕流。
//
// 只在「确实存在本站简体外挂字幕」时才动手：没有简体轨的片子（例如还没补
// 字幕、或字幕站只有繁体）保持 Emby 原样，不能把繁体轨藏掉让用户没字幕看。
// 返回被设为默认的流索引（-1 表示没动）。
func preferSimplifiedSubtitles(ms map[string]any, itemID, sourceID, token, userID string) int {
	streams, _ := ms["MediaStreams"].([]any)
	if len(streams) == 0 {
		return -1
	}
	var target map[string]any
	var targetIdx int
	for _, raw := range streams {
		st, ok := raw.(map[string]any)
		if !ok || !isOurSimplifiedSubtitle(st) {
			continue
		}
		idx, ok := streamIndex(st)
		if !ok {
			continue
		}
		target, targetIdx = st, idx
		break
	}
	if target == nil {
		return -1
	}

	// 简体轨设默认，并给出能真正取到字幕的完整地址。
	target["IsDefault"] = true
	if t, _ := target["Title"].(string); strings.TrimSpace(t) == "" {
		target["Title"] = "简体中文（本站）"
	}
	if du := subtitleDeliveryURL(itemID, sourceID, token, userID, targetIdx); du != "" {
		target["DeliveryUrl"] = du
	}
	ms["DefaultSubtitleStreamIndex"] = targetIdx

	// 片源内嵌的繁体中文轨保留（用户想看仍可手动选），但标注清楚，
	// 免得字幕菜单里两条都叫 Chinese 分不出来。
	for _, raw := range streams {
		st, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if idx, ok := streamIndex(st); ok && idx == targetIdx {
			continue
		}
		if typ, _ := st["Type"].(string); !strings.EqualFold(typ, "Subtitle") {
			continue
		}
		if !isChineseLang(st["Language"]) {
			continue
		}
		title, _ := st["Title"].(string)
		lang, _ := st["Language"].(string)
		if looksTraditionalTitle(title, lang) {
			st["IsDefault"] = false
			if !strings.Contains(strings.ToLower(title), "繁") {
				st["Title"] = "繁体 · " + strings.TrimSpace(title)
			}
		}
	}
	return targetIdx
}

// rewriteSimpleSubtitlePath 把简化字幕路径补上 MediaSourceId 段。
//
// sourceID 优先取请求里的 MediaSourceId 参数；没有就用 PlaybackInfo 阶段
// 记下的（客户端播放前必然先问 PlaybackInfo，缓存命中率接近 100%）。两者
// 都没有就返回 false，原样反代 —— 最多维持原来的 404，不会更糟。
func (g *Gateway) rewriteSimpleSubtitlePath(r *http.Request) bool {
	m := reSimpleSubtitle.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return false
	}
	itemID := m[1]
	sourceID := queryFold(r, "MediaSourceId")
	if sourceID == "" {
		sourceID = g.sourceIDFor(itemID)
	}
	if sourceID == "" {
		return false
	}
	// 在 /Subtitles/ 前面插入 /{sourceID}，其余路径与查询串一个字节不动。
	i := strings.Index(strings.ToLower(r.URL.Path), "/subtitles/")
	if i < 0 {
		return false
	}
	r.URL.Path = r.URL.Path[:i] + "/" + url.PathEscape(sourceID) + r.URL.Path[i:]
	// 改了 Path 之后旧的 RawPath 就对不上了，交给 net/url 重新编码。
	r.URL.RawPath = ""
	return true
}

func (g *Gateway) rememberSourceID(itemID, sourceID string) {
	if itemID == "" || sourceID == "" {
		return
	}
	g.srcMu.Lock()
	if g.sourceIDs == nil {
		g.sourceIDs = map[string]string{}
	}
	g.sourceIDs[itemID] = sourceID
	g.srcMu.Unlock()
}

func (g *Gateway) sourceIDFor(itemID string) string {
	g.srcMu.RLock()
	v := g.sourceIDs[itemID]
	g.srcMu.RUnlock()
	return v
}
