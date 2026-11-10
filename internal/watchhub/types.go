package watchhub

import (
	"bytes"
	"encoding/json"
)

// FlexString 兼容接口里字符串 / 数字 / null 三种写法（year、imdbRating 等）。
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	*f = FlexString(string(b))
	return nil
}

func (f FlexString) String() string { return string(f) }

// StringList 兼容「字符串数组」与「单个字符串」两种写法。
type StringList []string

func (s *StringList) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*s = arr
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil && one != "" {
		*s = StringList{one}
		return nil
	}
	*s = nil
	return nil
}

// ResourceList 兼容 manifest.resources 为字符串数组或对象数组的新旧两种规范。
type ResourceList []string

func (r *ResourceList) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		var one string
		if json.Unmarshal(b, &one) == nil && one != "" {
			*r = ResourceList{one}
			return nil
		}
		*r = nil
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			out = append(out, s)
			continue
		}
		var obj struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(v, &obj) == nil {
			if obj.Name != "" {
				out = append(out, obj.Name)
			} else if obj.ID != "" {
				out = append(out, obj.ID)
			}
		}
	}
	*r = out
	return nil
}

// Catalog 是 manifest 里声明的目录（WatchHub 的 catalogs 为空）。
type Catalog struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Manifest 对应 https://watchhub.strem.io/manifest.json
type Manifest struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Description string       `json:"description"`
	Logo        string       `json:"logo"`
	Background  string       `json:"background,omitempty"`
	Resources   ResourceList `json:"resources"`
	Types       []string     `json:"types"`
	IDPrefixes  []string     `json:"idPrefixes"`
	Catalogs    []Catalog    `json:"catalogs"`
}

// Meta 是 Cinemeta 返回的影片元数据（名称、海报、IMDb ID 等）。
type Meta struct {
	ID          string     `json:"id"`
	IMDbID      string     `json:"imdb_id"`
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Year        FlexString `json:"year"`
	ReleaseInfo FlexString `json:"releaseInfo"`
	IMDbRating  FlexString `json:"imdbRating"`
	Runtime     FlexString `json:"runtime"`
	Description string     `json:"description"`
	Country     string     `json:"country"`
	Genres      StringList `json:"genres"`
	Genre       StringList `json:"genre"`
	Cast        StringList `json:"cast"`
	Director    StringList `json:"director"`
	Writer      StringList `json:"writer"`
	Slug        string     `json:"slug"`
	Poster      string     `json:"poster"`
	Background  string     `json:"background"`
	Logo        string     `json:"logo"`
	// Videos 是剧集的单集清单。一次 meta 请求就能拿到全季的集名与简介，
	// 不用为每一集单独请求——这一点直接决定了刮削会不会触发风控。
	Videos []Video `json:"videos"`
}

// Video 是剧集里的一集。ID 形如 `tt12590266:1:3`。
type Video struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Released string `json:"released"`
	Overview string `json:"overview"`
	Thumb    string `json:"thumbnail"`
}

// Stream 是 WatchHub /stream/{type}/{id}.json 里的一条播放源。
type Stream struct {
	Name         string `json:"name"`
	Title        string `json:"title"`
	ExternalURL  string `json:"externalUrl"`
	AndroidURL   string `json:"androidUrl"`
	AndroidTVURL string `json:"androidTvUrl"`
	IOSURL       string `json:"iosUrl"`
	TVOSURL      string `json:"tvOsUrl"`
	FireTVURL    string `json:"fireTvUrl"`
	RokuURL      string `json:"rokuUrl"`
	TizenURL     string `json:"tizenUrl"`
	WebOSURL     string `json:"webosUrl"`
	XboxURL      string `json:"xboxUrl"`
	TitanOSURL   string `json:"titanOsUrl"`
	VidaaOSURL   string `json:"vidaaOsUrl"`
}

// DeepLinks 收集非空的各平台深链。
func (s Stream) DeepLinks() map[string]string {
	pairs := []struct{ key, val string }{
		{"android", s.AndroidURL}, {"androidTv", s.AndroidTVURL},
		{"ios", s.IOSURL}, {"tvOs", s.TVOSURL}, {"fireTv", s.FireTVURL},
		{"roku", s.RokuURL}, {"tizen", s.TizenURL}, {"webos", s.WebOSURL},
		{"xbox", s.XboxURL}, {"titanOs", s.TitanOSURL}, {"vidaaOs", s.VidaaOSURL},
	}
	m := make(map[string]string)
	for _, p := range pairs {
		if p.val != "" {
			m[p.key] = p.val
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

type StreamResponse struct {
	Streams   []Stream `json:"streams"`
	Copyright string   `json:"copyright,omitempty"`
}

// Source 是输出到结果文件里的精简播放源。
type Source struct {
	Platform     string            `json:"platform"`
	Availability string            `json:"availability,omitempty"`
	URL          string            `json:"url,omitempty"`
	DeepLinks    map[string]string `json:"deep_links,omitempty"`
}

// Item 是一条影片的完整输出。
type Item struct {
	IMDbID         string   `json:"imdb_id"`
	Name           string   `json:"name,omitempty"`
	Type           string   `json:"type"`
	Year           string   `json:"year,omitempty"`
	ReleaseInfo    string   `json:"release_info,omitempty"`
	IMDbRating     string   `json:"imdb_rating,omitempty"`
	Runtime        string   `json:"runtime,omitempty"`
	Genres         []string `json:"genres,omitempty"`
	Cast           []string `json:"cast,omitempty"`
	Director       []string `json:"director,omitempty"`
	Description    string   `json:"description,omitempty"`
	Slug           string   `json:"slug,omitempty"`
	Poster         string   `json:"poster,omitempty"`
	Background     string   `json:"background,omitempty"`
	Logo           string   `json:"logo,omitempty"`
	PosterFile     string   `json:"poster_file,omitempty"`
	BackgroundFile string   `json:"background_file,omitempty"`
	LogoFile       string   `json:"logo_file,omitempty"`
	PlatformCount  int      `json:"platform_count"`
	Platforms      []string `json:"platforms,omitempty"`
	// Videos 是剧集的单集清单，刮削写 episodedetails 时用。
	Videos   []Video  `json:"videos,omitempty"`
	WatchURL string   `json:"watch_url,omitempty"`
	Streams  []Source `json:"streams,omitempty"`
	Error    string   `json:"error,omitempty"`
}

type AddonInfo struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Version         string    `json:"version"`
	Description     string    `json:"description,omitempty"`
	ManifestURL     string    `json:"manifest_url"`
	LogoURL         string    `json:"logo_url,omitempty"`
	LogoFile        string    `json:"logo_file,omitempty"`
	Resources       []string  `json:"resources"`
	Types           []string  `json:"types"`
	IDPrefixes      []string  `json:"id_prefixes"`
	Catalogs        []Catalog `json:"catalogs"`
	MetaSource      string    `json:"meta_source"`
	CopyrightNotice string    `json:"copyright_notice,omitempty"`
}

type QueryInfo struct {
	Type       string `json:"type"`
	Mode       string `json:"mode"`
	Limit      int    `json:"limit,omitempty"`
	PosterSize string `json:"poster_size"`
	Images     string `json:"images"`
}

type Report struct {
	GeneratedAt string    `json:"generated_at"`
	Addon       AddonInfo `json:"addon"`
	Query       QueryInfo `json:"query"`
	Count       int       `json:"count"`
	Items       []Item    `json:"items"`
}
