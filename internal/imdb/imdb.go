package imdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"strm115/internal/netproxy"
)

// SearchResult 表示一条 IMDb 搜索结果
type SearchResult struct {
	IMDbID      string `json:"imdbID"`      // 如 tt1375666
	Title       string `json:"title"`       // 正式名称
	Year        string `json:"year"`        // 年份
	Type        string `json:"type"`        // movie, series, episode
	PosterURL   string `json:"posterURL"`   // 海报图 URL
	Rating      string `json:"rating"`      // 评分
	Description string `json:"description"` // 简介
}

// SearchResponse 是搜索响应
type SearchResponse struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"`
	Error   string         `json:"error,omitempty"`
	Source  string         `json:"source"` // "official" 或 "suggestion"
}

// ──────────────────────────────────────────────
// Client
// ──────────────────────────────────────────────

// OfficialAPIEndpoint 是 IMDb 官方 GraphQL API 端点。
// 声明为变量（而非常量）便于在测试中注入 mock 服务器地址。
var OfficialAPIEndpoint = "https://api.imdb.com/graphql"

// Client 是 IMDb 搜索客户端
type Client struct {
	httpClient  *http.Client
	apiKey      string // IMDb 官方 API Key（通过 AWS Data Exchange 获取）
	useOfficial bool   // 是否使用官方 GraphQL API
}

// NewClient 创建一个 IMDb 搜索客户端
//
//   - apiKey 不为空时：使用 IMDb 官方 GraphQL API（https://api.imdb.com/graphql）
//     apiKey 通过 AWS Data Exchange 订阅获取，放入请求头 Authorization: Bearer <apiKey>
//   - apiKey 为空时：降级使用 IMDb 免费自动建议接口（无需 Key）
func NewClient(apiKey string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{Proxy: netproxy.URL},
		},
		apiKey:      apiKey,
		useOfficial: apiKey != "",
	}
}

// NewClientWithProxy 和 NewClient 一样，但走指定的出站代理。
//
// 设置里配了代理时，IMDb 换英文名和「检测 Key」都得跟着走：
// 直连不通的环境里，不走代理会把「网络不通」谎报成「Key 无效」。
// proxy 留空就等同于 NewClient。
func NewClientWithProxy(apiKey, proxy string) *Client {
	c := NewClient(apiKey)
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return c
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return c
	}
	c.httpClient.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
	return c
}

// Search 根据名称搜索 IMDb，返回搜索结果
func (c *Client) Search(name string) (*SearchResponse, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("搜索名称不能为空")
	}

	if c.useOfficial {
		return c.searchViaOfficialAPI(name)
	}
	return c.searchViaSuggestionAPI(name)
}

// GetByIMDbID 根据 IMDb ID 获取详情
func (c *Client) GetByIMDbID(imdbID string) (*SearchResult, error) {
	if !strings.HasPrefix(imdbID, "tt") {
		return nil, fmt.Errorf("IMDb ID 必须以 tt 开头")
	}

	if c.useOfficial {
		return c.getByIDViaOfficialAPI(imdbID)
	}
	return c.getByIDViaSuggestion(imdbID)
}

// ──────────────────────────────────────────────
// 方式一：IMDb 官方 GraphQL API（用户提供 API Key）
// 端点: https://api.imdb.com/graphql
// 认证: Authorization: Bearer <apiKey>
// ──────────────────────────────────────────────

// graphqlRequest 是 GraphQL 请求体
type graphqlRequest struct {
	Query         string                 `json:"query"`
	Variables     map[string]interface{} `json:"variables"`
	OperationName string                 `json:"operationName,omitempty"`
}

// graphqlResponse 是 GraphQL 响应体
type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *Client) sendGraphQL(query string, variables map[string]interface{}) ([]byte, error) {
	reqBody := graphqlRequest{
		Query:     query,
		Variables: variables,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化 GraphQL 请求失败: %w", err)
	}

	req, err := http.NewRequest("POST", OfficialAPIEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// IMDb 官方 API 经 AWS Data Exchange 分发，鉴权方式随订阅形态不同：
	// API Gateway 常用 x-api-key，部分网关用 Bearer Token。两者同时发送，
	// 网关会忽略不认识的那个；如你的订阅使用 AWS SigV4，需在此扩展签名逻辑。
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("User-Agent", "offline-link-finder/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 IMDb 官方 API 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("IMDb 官方 API 返回错误状态码 %d: %s", resp.StatusCode, string(body))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	// 检查 GraphQL 错误
	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("解析 GraphQL 响应失败: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		errMsgs := make([]string, len(gqlResp.Errors))
		for i, e := range gqlResp.Errors {
			errMsgs[i] = e.Message
		}
		return nil, fmt.Errorf("GraphQL 错误: %s", strings.Join(errMsgs, "; "))
	}

	return gqlResp.Data, nil
}

// searchViaOfficialAPI 使用官方 GraphQL API 按名称搜索
func (c *Client) searchViaOfficialAPI(name string) (*SearchResponse, error) {
	// IMDb 官方 GraphQL 搜索查询
	// 使用 titleSearch 查询，返回匹配的标题列表
	query := `
query SearchTitles($query: String!) {
  titleSearch(input: { query: $query }) {
    results {
      id
      titleText {
        text
      }
      releaseYear {
        year
      }
      titleType {
        text
      }
      primaryImage {
        url
      }
      ratingsSummary {
        aggregateRating
      }
    }
  }
}`

	data, err := c.sendGraphQL(query, map[string]interface{}{
		"query": name,
	})
	if err != nil {
		return nil, err
	}

	// 解析搜索结果
	var searchResult struct {
		TitleSearch struct {
			Results []struct {
				ID        string `json:"id"`
				TitleText struct {
					Text string `json:"text"`
				} `json:"titleText"`
				ReleaseYear struct {
					Year int `json:"year"`
				} `json:"releaseYear"`
				TitleType struct {
					Text string `json:"text"`
				} `json:"titleType"`
				PrimaryImage struct {
					URL string `json:"url"`
				} `json:"primaryImage"`
				RatingsSummary struct {
					AggregateRating float64 `json:"aggregateRating"`
				} `json:"ratingsSummary"`
			} `json:"results"`
		} `json:"titleSearch"`
	}

	if err := json.Unmarshal(data, &searchResult); err != nil {
		return nil, fmt.Errorf("解析搜索结果失败: %w", err)
	}

	results := make([]SearchResult, 0, len(searchResult.TitleSearch.Results))
	for _, r := range searchResult.TitleSearch.Results {
		yearStr := ""
		if r.ReleaseYear.Year > 0 {
			yearStr = fmt.Sprintf("%d", r.ReleaseYear.Year)
		}
		ratingStr := ""
		if r.RatingsSummary.AggregateRating > 0 {
			ratingStr = fmt.Sprintf("%.1f", r.RatingsSummary.AggregateRating)
		}
		results = append(results, SearchResult{
			IMDbID:    r.ID,
			Title:     r.TitleText.Text,
			Year:      yearStr,
			Type:      r.TitleType.Text,
			PosterURL: r.PrimaryImage.URL,
			Rating:    ratingStr,
		})
	}

	return &SearchResponse{
		Query:   name,
		Results: results,
		Total:   len(results),
		Source:  "official",
	}, nil
}

// getByIDViaOfficialAPI 使用官方 GraphQL API 按 IMDb ID 获取详情
func (c *Client) getByIDViaOfficialAPI(imdbID string) (*SearchResult, error) {
	query := `
query GetTitle($id: ID!) {
  title(id: $id) {
    id
    titleText {
      text
    }
    releaseYear {
      year
    }
    titleType {
      text
    }
    primaryImage {
      url
    }
    ratingsSummary {
      aggregateRating
    }
    plot {
      plainText
    }
  }
}`

	data, err := c.sendGraphQL(query, map[string]interface{}{
		"id": imdbID,
	})
	if err != nil {
		return nil, err
	}

	var titleResult struct {
		Title struct {
			ID        string `json:"id"`
			TitleText struct {
				Text string `json:"text"`
			} `json:"titleText"`
			ReleaseYear struct {
				Year int `json:"year"`
			} `json:"releaseYear"`
			TitleType struct {
				Text string `json:"text"`
			} `json:"titleType"`
			PrimaryImage struct {
				URL string `json:"url"`
			} `json:"primaryImage"`
			RatingsSummary struct {
				AggregateRating float64 `json:"aggregateRating"`
			} `json:"ratingsSummary"`
			Plot struct {
				PlainText string `json:"plainText"`
			} `json:"plot"`
		} `json:"title"`
	}

	if err := json.Unmarshal(data, &titleResult); err != nil {
		return nil, fmt.Errorf("解析标题详情失败: %w", err)
	}

	if titleResult.Title.ID == "" {
		return nil, fmt.Errorf("未找到 IMDb ID: %s", imdbID)
	}

	t := titleResult.Title
	yearStr := ""
	if t.ReleaseYear.Year > 0 {
		yearStr = fmt.Sprintf("%d", t.ReleaseYear.Year)
	}
	ratingStr := ""
	if t.RatingsSummary.AggregateRating > 0 {
		ratingStr = fmt.Sprintf("%.1f", t.RatingsSummary.AggregateRating)
	}

	return &SearchResult{
		IMDbID:      t.ID,
		Title:       t.TitleText.Text,
		Year:        yearStr,
		Type:        t.TitleType.Text,
		PosterURL:   t.PrimaryImage.URL,
		Rating:      ratingStr,
		Description: t.Plot.PlainText,
	}, nil
}

// ──────────────────────────────────────────────
// 方式二：IMDb 免费 Suggestion API（无需 Key，降级方案）
// 端点: https://v3.sg.media-imdb.com/suggestion/<首字母>/<搜索词>.json
// ──────────────────────────────────────────────

// SuggestionAPIEndpoint 是 IMDb 免费自动建议接口模板（无需 API Key）。
// 声明为变量便于在测试中注入 mock 服务器地址。
var SuggestionAPIEndpoint = "https://v3.sg.media-imdb.com/suggestion/%s/%s.json"

// suggestionItem 映射 IMDb suggestion API v1 的返回结构。
// 实测响应示例：
//
//	{"d":[{"id":"tt1375666","l":"Inception","y":2010,"tl":"2010",
//	       "qid":"movie","q":"feature","s":"Leonardo DiCaprio, ...",
//	       "i":{"imageUrl":"https://..."}}],"q":"tt1375666","v":1}
//
// 同时保留旧版字段（text / kind / image.url）以兼容接口变更。
type suggestionItem struct {
	ID   string `json:"id"`
	L    string `json:"l"`    // 正式名称（v1）
	Y    int    `json:"y"`    // 年份（v1，数字）
	TL   string `json:"tl"`   // 年份文本，如 "2010" / "2010 Video"
	QID  string `json:"qid"`  // 类型：movie / tvSeries / video / short
	Q    string `json:"q"`    // 分类：feature / video / TV series
	S    string `json:"s"`    // 副标题（主演等）
	Text string `json:"text"` // 旧版：如 "The Shawshank Redemption (1994)"
	Kind string `json:"kind"` // 旧版分类
	I    struct {
		ImageURL string `json:"imageUrl"`
	} `json:"i"`
	Image struct {
		URL string `json:"url"`
	} `json:"image"`
}

type suggestionResponse struct {
	D []suggestionItem `json:"d"`
}

// toSearchResult 把新旧两种响应格式统一转换为 SearchResult
func (s suggestionItem) toSearchResult() SearchResult {
	title, year := s.L, ""

	// 兼容旧版 text 字段
	if title == "" && s.Text != "" {
		title, year = parseTitleAndYear(s.Text)
	}

	// 年份优先级：y (数字) > tl (文本) > 旧版 text 中解析出的
	if year == "" {
		if s.Y > 0 {
			year = strconv.Itoa(s.Y)
		} else {
			year = s.TL
		}
	}

	titleType := s.QID
	if titleType == "" {
		titleType = s.Q
	}
	if titleType == "" {
		titleType = s.Kind
	}
	if titleType == "" {
		titleType = "title"
	}

	poster := s.I.ImageURL
	if poster == "" {
		poster = s.Image.URL
	}

	return SearchResult{
		IMDbID:    s.ID,
		Title:     title,
		Year:      year,
		Type:      titleType,
		PosterURL: poster,
	}
}

// fetchSuggestions 调用免费 suggestion 接口
func (c *Client) fetchSuggestions(term string) (*suggestionResponse, error) {
	firstChar := strings.ToLower(string([]rune(term)[0]))
	if firstChar < "a" || firstChar > "z" {
		firstChar = "x" // 非字母开头统一归入 x 分区
	}

	// 路径中的空格用 %20，避免 + 被当作字面加号
	encoded := strings.ReplaceAll(url.QueryEscape(strings.ToLower(term)), "+", "%20")
	endpoint := fmt.Sprintf(SuggestionAPIEndpoint, firstChar, encoded)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 IMDb 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("IMDb 返回错误状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var suggResp suggestionResponse
	if err := json.Unmarshal(body, &suggResp); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	return &suggResp, nil
}

func (c *Client) searchViaSuggestionAPI(name string) (*SearchResponse, error) {
	suggResp, err := c.fetchSuggestions(name)
	if err != nil {
		return nil, err
	}

	results := make([]SearchResult, 0, len(suggResp.D))
	for _, item := range suggResp.D {
		if !strings.HasPrefix(item.ID, "tt") {
			continue // 只保留标题（tt），过滤演员（nm）等
		}
		results = append(results, item.toSearchResult())
	}

	return &SearchResponse{
		Query:   name,
		Results: results,
		Total:   len(results),
		Source:  "suggestion",
	}, nil
}

func (c *Client) getByIDViaSuggestion(imdbID string) (*SearchResult, error) {
	suggResp, err := c.fetchSuggestions(imdbID)
	if err != nil {
		return nil, err
	}

	for _, item := range suggResp.D {
		if item.ID == imdbID {
			r := item.toSearchResult()
			return &r, nil
		}
	}

	return nil, fmt.Errorf("未找到 IMDb ID: %s", imdbID)
}

// ──────────────────────────────────────────────
// 辅助函数
// ──────────────────────────────────────────────

// parseTitleAndYear 从 "The Shawshank Redemption (1994)" 格式中解析标题和年份
func parseTitleAndYear(text string) (title, year string) {
	startIdx := strings.LastIndex(text, "(")
	if startIdx == -1 {
		return strings.TrimSpace(text), ""
	}
	endIdx := strings.LastIndex(text, ")")
	if endIdx == -1 || endIdx < startIdx {
		return strings.TrimSpace(text), ""
	}
	title = strings.TrimSpace(text[:startIdx])
	year = text[startIdx+1 : endIdx]
	return title, year
}
