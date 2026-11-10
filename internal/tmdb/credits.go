package tmdb

// 演员表。
//
// 为什么从 TMDB 拿而不是从 IMDb / Cinemeta（我们原先的数据源）：
//
//  1. **中文名**。TMDB 的 credits 接口带 language=zh-CN，返回的就是「梁家辉」
//     而不是 "Tony Ka Fai Leung"。IMDb 的演员名永远是拉丁字母，中文名得自己
//     维护一张对照表，而这类表永远不够用。
//  2. **头像**。profile_path 直接是图片路径。Emby 读 nfo 的 <actor><thumb>
//     时会去下载这张图，于是演员头像就有了——我们的 nfo 之前没有 <thumb>，
//     所以 Emby 里永远是灰色占位图。
//
// 两个接口的坑：
//
//  1. 剧集要用 aggregate_credits，它的角色在 roles[0].character 里，
//     不像电影那样直接给 character。
//  2. profile_path 可能为 null（配角、素人），这时不写 <thumb>，
//     让 Emby 显示占位图，而不是写一个 404 的地址进去。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ProfileSize 是演员头像的档位。w185 约 185px 宽，Emby 的演员横排用这个够清楚，
// 一个 15 人的演员表也就 2MB 出头。
const ProfileSize = "w185"

// CastMember 是演员表里的一位。
type CastMember struct {
	Name  string `json:"name"`
	Role  string `json:"role,omitempty"`  // 角色名
	Image string `json:"image,omitempty"` // 头像完整地址；没有头像时为空
	// ID 是 TMDB 的 person id。留着是为了以后能按人聚合，
	// 眼下的用途只有一个：日志里能对上人。
	ID int `json:"id,omitempty"`
}

// Find 用 IMDb ID 反查 TMDB 的条目。
//
// 刮削链路是「先有 IMDb ID，再要中文」：文件名解析出来的就是 IMDb ID，
// 拿它换 TMDB 是最准的一步（比按片名搜靠谱得多，同名重拍片不会认错）。
//
// 两个 id 一起返回，是因为 **IMDb ID 在 TMDB 上不一定对得上同一种类型**：
// tt12590266（赛博朋克：边缘行者 第一季）在 TMDB 被挂在
// 「Cyberpunk: Edgerunners II」这部**电影**下面，而不是 TV 条目。
// 只回一个 id 的话，剧集就会拿着电影 id 去请求 /tv/…/aggregate_credits，
// 结果是 404，演员表静默变空 —— 表现得像「这部剧没有演员」，很难查。
func (c *Client) Find(ctx context.Context, imdbID string) (movieID, tvID int, err error) {
	imdbID = strings.TrimSpace(imdbID)
	if imdbID == "" {
		return 0, 0, fmt.Errorf("没有 IMDb ID，无法反查 TMDB")
	}
	raw, err := c.get(ctx, "/find/"+imdbID, map[string][]string{
		"external_source": {"imdb_id"},
	})
	if err != nil {
		return 0, 0, err
	}
	var out struct {
		Movies []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
		TV []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, fmt.Errorf("解析 TMDB find 结果失败: %w", err)
	}
	if len(out.Movies) > 0 {
		movieID = out.Movies[0].ID
	}
	if len(out.TV) > 0 {
		tvID = out.TV[0].ID
	}
	if movieID == 0 && tvID == 0 {
		return 0, 0, fmt.Errorf("TMDB 里没有对应的条目（%s）", imdbID)
	}
	return movieID, tvID, nil
}

// Credits 是一次演员 / 导演查询的结果。
//
// 两个放一起返回，因为它们在同一个响应里：为了拿中文导演名再打一次同一个
// 接口，等于把每部片的请求量翻倍。
type Credits struct {
	Cast      []CastMember
	Directors []string
}

// Credits 取演员表与导演。limit 为 0 时取 15 位，够填满 Emby 的演员横排。
func (c *Client) Credits(ctx context.Context, kind string, id, limit int) (*Credits, error) {
	if id <= 0 {
		return nil, fmt.Errorf("缺少 TMDB ID")
	}
	if limit <= 0 {
		limit = 15
	}
	if limit > 40 {
		limit = 40
	}
	path := "/movie/" + strconv.Itoa(id) + "/credits"
	if kind == KindTV {
		// 剧集用 aggregate：跨季合并，主角不会因为「只在第一季出现」而漏掉。
		path = "/tv/" + strconv.Itoa(id) + "/aggregate_credits"
	}
	raw, err := c.get(ctx, path, map[string][]string{"language": {DefaultLanguage}})
	if err != nil {
		return nil, err
	}

	var out struct {
		Cast []struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			Character   string `json:"character"`
			ProfilePath string `json:"profile_path"`
			Order       int    `json:"order"`
			Roles       []struct {
				Character string `json:"character"`
			} `json:"roles"`
		} `json:"cast"`
		Crew []struct {
			Name string `json:"name"`
			Job  string `json:"job"`
			// 剧集走 aggregate_credits，职位在 jobs[] 里而不是 job。
			Jobs []struct {
				Job string `json:"job"`
			} `json:"jobs"`
		} `json:"crew"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 TMDB 演员表失败: %w", err)
	}

	seen := make(map[string]bool, len(out.Cast))
	result := make([]CastMember, 0, limit)
	for _, p := range out.Cast {
		name := strings.TrimSpace(p.Name)
		if name == "" || seen[name] {
			continue
		}
		role := strings.TrimSpace(p.Character)
		if role == "" && len(p.Roles) > 0 {
			role = strings.TrimSpace(p.Roles[0].Character)
		}
		seen[name] = true
		result = append(result, CastMember{
			Name:  name,
			Role:  role,
			Image: imageURL(p.ProfilePath, ProfileSize),
			ID:    p.ID,
		})
		if len(result) >= limit {
			break
		}
	}
	return &Credits{Cast: result, Directors: directorsOf(out.Crew)}, nil
}

// directorsOf 挑出导演。
//
// 剧集有一个坑：aggregate_credits 的 crew 是**按集合并**的，一部日番能有
// 八九个「导演」（每集的分镜/演出都被算进去）。全写进 tvshow.nfo，Emby 的
// 导演一栏就会列出九个人，明显不对。所以：
//
//   - 有 Creator（剧集主创）就只用 Creator，那才是「这部剧是谁做的」；
//   - 没有就退回 Director，最多 3 个 —— 真人剧常见的是导演 + 副导演。
func directorsOf(crew []struct {
	Name string `json:"name"`
	Job  string `json:"job"`
	Jobs []struct {
		Job string `json:"job"`
	} `json:"jobs"`
}) []string {
	var creators, directors []string
	seenC := map[string]bool{}
	seenD := map[string]bool{}
	for _, p := range crew {
		job := p.Job
		if job == "" && len(p.Jobs) > 0 {
			job = p.Jobs[0].Job
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		switch job {
		case "Creator":
			if !seenC[name] {
				seenC[name] = true
				creators = append(creators, name)
			}
		case "Director":
			if !seenD[name] {
				seenD[name] = true
				directors = append(directors, name)
			}
		}
	}
	if len(creators) > 0 {
		return creators
	}
	if len(directors) > 3 {
		directors = directors[:3]
	}
	return directors
}
