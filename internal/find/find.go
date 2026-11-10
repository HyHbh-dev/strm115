// Package find 把「一段文本」变成「这部片是什么、在哪能看、能不能离线下载」。
//
// 它串起三件事：
//
//	linkparse  找出磁力 / ed2k / 种子 / IMDb 链接
//	imdb       没有链接时按片名搜到正式名称与 IMDb ID
//	watchhub   用 IMDb ID 取海报、评分、简介和各平台观看入口
//
// 设计原则：**任何一步失败都不该让整次查询失败**。用户粘一段磁力链进来，
// 就算 IMDb 挂了也得把链接还给他 —— 那才是他真正要提交给 115 的东西。
package find

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"time"

	"strm115/internal/imdb"
	"strm115/internal/linkparse"
	"strm115/internal/watchhub"
)

// Result 是一次解析的全部产出。
type Result struct {
	Text      string                  `json:"text"`
	HasLinks  bool                    `json:"has_links"`
	Links     []linkparse.MatchedLink `json:"links,omitempty"`
	MediaName string                  `json:"media_name,omitempty"`
	IMDbID    string                  `json:"imdb_id,omitempty"`
	IMDb      *imdb.SearchResponse    `json:"imdb,omitempty"`
	Watch     *watchhub.Item          `json:"watch,omitempty"`
	Notes     []string                `json:"notes,omitempty"`
}

// Options 控制外部调用。
type Options struct {
	IMDbKey string        // IMDb 官方 API Key；空 = 用免费 suggestion 接口
	Timeout time.Duration // 整体超时；0 = 30 秒
}

// ErrNoInput 输入里既没有链接也提不出片名。
var ErrNoInput = errNoInput{}

type errNoInput struct{}

func (errNoInput) Error() string { return "没能从这段文本里认出链接或片名" }

// Analyze 解析一段文本。
func Analyze(ctx context.Context, text string, opt Options) (*Result, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrNoInput
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	parsed := linkparse.Parse(text)
	res := &Result{
		Text:     text,
		Links:    parsed.FoundLinks,
		HasLinks: len(parsed.FoundLinks) > 0,
	}

	// 片名：优先用文本里独立出现的名字；有链接时退而求其次从 dn= 里捞。
	name := parsed.MediaName
	if name == "" {
		name = nameFromLinks(parsed.FoundLinks)
	}
	res.MediaName = name

	// IMDb ID：链接里带的（tt 页面 / find?q= 的搜索结果）最可信。
	id := parsed.ExtractID()
	res.IMDbID = id
	searchTerm := ""
	for _, l := range parsed.FoundLinks {
		if l.SearchTerm != "" {
			searchTerm = l.SearchTerm
			break
		}
	}

	client := imdb.NewClient(opt.IMDbKey)

	// 没有 ID 但有名字 → 搜一次，取第一条当主结果。
	if id == "" {
		term := name
		if term == "" {
			term = searchTerm
		}
		if term != "" {
			resp, err := client.Search(term)
			if err != nil {
				res.Notes = append(res.Notes, "IMDb 搜索失败："+err.Error())
			} else {
				if len(resp.Results) == 0 {
					// 文件名常带年份，会拉低召回；去掉尾部年份再试一次。
					if without := stripYear(term); without != "" && without != term {
						if retry, retryErr := client.Search(without); retryErr == nil && len(retry.Results) > 0 {
							resp = retry
						}
					}
				}
				res.IMDb = resp
				if len(resp.Results) > 0 {
					res.IMDbID = resp.Results[0].IMDbID
				}
			}
		}
	}

	if res.IMDbID == "" {
		if !res.HasLinks && res.MediaName == "" {
			return res, ErrNoInput
		}
		return res, nil
	}

	// 有了 ID 就去查「在哪能看」。watchhub 挂了只是少一块信息，不影响链接。
	typ := ""
	if res.IMDb != nil && len(res.IMDb.Results) > 0 {
		typ = stremioType(res.IMDb.Results[0].Type)
	}
	item, err := watchDetail(ctx, typ, res.IMDbID)
	if err != nil {
		res.Notes = append(res.Notes, "查询观看渠道失败："+err.Error())
	} else {
		res.Watch = item
	}
	return res, nil
}

// watchDetail 是查「在哪能看」的接缝。默认走真实的 WatchHub 客户端，
// 测试把它换掉就能在不出网的情况下验证整条链路。
var watchDetail = func(ctx context.Context, typ, id string) (*watchhub.Item, error) {
	return watchhub.NewClient().Detail(ctx, typ, id)
}

var (
	reMagnetDN = regexp.MustCompile(`(?i)[?&]dn=([^&]+)`)
	reTailYear = regexp.MustCompile(`\s+(19|20)\d{2}$`)
)

// nameFromLinks 从磁力链的 dn= 参数里捞文件名。这是整段文本里最像片名的东西。
func nameFromLinks(links []linkparse.MatchedLink) string {
	for _, l := range links {
		if l.Type != linkparse.LinkTypeMagnet {
			continue
		}
		m := reMagnetDN.FindStringSubmatch(l.Value)
		if len(m) != 2 {
			continue
		}
		raw, err := url.QueryUnescape(m[1])
		if err != nil || strings.TrimSpace(raw) == "" {
			continue
		}
		if name := linkparse.CleanMediaName(raw); name != "" {
			return name
		}
	}
	return ""
}

func stripYear(s string) string {
	return strings.TrimSpace(reTailYear.ReplaceAllString(s, ""))
}

// stremioType 把 IMDb 的类型名映射成 Stremio 侧的类型。
// 认不出来就返回空串 —— 让 watchhub 用 manifest 默认类型去试，比猜错强。
func stremioType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "movie", "feature", "video", "tv movie", "tvmovie":
		return "movie"
	case "series", "tvseries", "tv series", "tvminiseries", "tv mini series", "episode", "tv series episode":
		return "series"
	}
	return ""
}
