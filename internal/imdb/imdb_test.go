package imdb

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTitleAndYear(t *testing.T) {
	cases := []struct {
		in        string
		wantTitle string
		wantYear  string
	}{
		{"The Shawshank Redemption (1994)", "The Shawshank Redemption", "1994"},
		{"Inception (2010)", "Inception", "2010"},
		{"Interstellar", "Interstellar", ""},
		{"Spider-Man: Into the Spider-Verse (2018)", "Spider-Man: Into the Spider-Verse", "2018"},
	}
	for _, c := range cases {
		title, year := parseTitleAndYear(c.in)
		if title != c.wantTitle || year != c.wantYear {
			t.Errorf("parseTitleAndYear(%q) = (%q, %q), 期望 (%q, %q)",
				c.in, title, year, c.wantTitle, c.wantYear)
		}
	}
}

func TestNewClientModeSelection(t *testing.T) {
	if NewClient("").useOfficial {
		t.Error("空 API Key 应使用免费 Suggestion 接口")
	}
	if !NewClient("my-key").useOfficial {
		t.Error("提供 API Key 应使用官方 GraphQL API")
	}
}

func TestSearchRejectsEmptyName(t *testing.T) {
	for _, c := range []*Client{NewClient(""), NewClient("key")} {
		if _, err := c.Search("   "); err == nil {
			t.Error("空名称应返回错误")
		}
	}
}

func TestGetByIMDbIDRejectsInvalidID(t *testing.T) {
	if _, err := NewClient("key").GetByIMDbID("nm0000158"); err == nil {
		t.Error("非 tt 开头的 ID 应返回错误")
	}
}

// TestSearchViaOfficialAPI 用 mock 服务器验证 GraphQL 请求头、请求体与响应解析
func TestSearchViaOfficialAPI(t *testing.T) {
	var capturedQuery string
	var capturedAuth string
	var capturedVars map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("应为 POST，实际 %s", r.Method)
		}
		capturedAuth = r.Header.Get("Authorization")

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求体解析失败: %v", err)
		}
		capturedQuery = req.Query
		capturedVars = req.Variables

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"titleSearch": {
					"results": [
						{
							"id": "tt1375666",
							"titleText": {"text": "Inception"},
							"releaseYear": {"year": 2010},
							"titleType": {"text": "Movie"},
							"primaryImage": {"url": "https://img.example/p.jpg"},
							"ratingsSummary": {"aggregateRating": 8.8}
						}
					]
				}
			}
		}`))
	}))
	defer server.Close()

	old := OfficialAPIEndpoint
	OfficialAPIEndpoint = server.URL
	defer func() { OfficialAPIEndpoint = old }()

	client := NewClient("test-api-key")
	resp, err := client.Search("Inception")
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	if capturedAuth != "Bearer test-api-key" {
		t.Errorf("Authorization 头错误: %q", capturedAuth)
	}
	if !strings.Contains(capturedQuery, "titleSearch") {
		t.Errorf("GraphQL query 缺少 titleSearch: %s", capturedQuery)
	}
	if capturedVars["query"] != "Inception" {
		t.Errorf("变量 query 错误: %v", capturedVars["query"])
	}
	if resp.Source != "official" {
		t.Errorf("Source 应为 official，实际 %s", resp.Source)
	}
	if resp.Total != 1 || len(resp.Results) != 1 {
		t.Fatalf("结果数量错误: %+v", resp)
	}

	got := resp.Results[0]
	if got.IMDbID != "tt1375666" || got.Title != "Inception" || got.Year != "2010" {
		t.Errorf("结果字段错误: %+v", got)
	}
	if got.Rating != "8.8" {
		t.Errorf("评分格式化错误: %q", got.Rating)
	}
	if got.PosterURL != "https://img.example/p.jpg" {
		t.Errorf("海报 URL 错误: %q", got.PosterURL)
	}
}

// TestOfficialAPIGraphQLError 验证 GraphQL errors 被正确上报
func TestOfficialAPIGraphQLError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": null, "errors": [{"message": "Invalid API key"}]}`))
	}))
	defer server.Close()

	old := OfficialAPIEndpoint
	OfficialAPIEndpoint = server.URL
	defer func() { OfficialAPIEndpoint = old }()

	if _, err := NewClient("bad-key").Search("X"); err == nil {
		t.Error("GraphQL errors 应返回错误")
	} else if !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("错误信息未包含原始消息: %v", err)
	}
}

// TestSuggestionAPIV1Schema 验证免费降级路径能解析 IMDb suggestion v1 的真实字段
func TestSuggestionAPIV1Schema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"d":[
			{"i":{"imageUrl":"https://img.example/inception.jpg","height":1037,"width":700},
			 "id":"tt1375666","l":"Inception","q":"feature","qid":"movie",
			 "rank":306,"s":"Leonardo DiCaprio","tl":"2010","y":2010},
			{"id":"nm0000138","l":"Leonardo DiCaprio","qid":"name"}
		],"q":"inception","v":1}`))
	}))
	defer server.Close()

	old := SuggestionAPIEndpoint
	SuggestionAPIEndpoint = server.URL + "/suggestion/%s/%s.json"
	defer func() { SuggestionAPIEndpoint = old }()

	resp, err := NewClient("").Search("Inception")
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if resp.Source != "suggestion" {
		t.Errorf("Source = %q", resp.Source)
	}
	if resp.Total != 1 {
		t.Fatalf("应过滤掉 nm 开头的演员条目，实际 %d 条: %+v", resp.Total, resp.Results)
	}
	got := resp.Results[0]
	if got.IMDbID != "tt1375666" || got.Title != "Inception" ||
		got.Year != "2010" || got.Type != "movie" ||
		got.PosterURL != "https://img.example/inception.jpg" {
		t.Errorf("字段解析错误: %+v", got)
	}
}

// TestGetByIDViaSuggestion 验证无 Key 时按 tt ID 也能拿到正式名称
func TestGetByIDViaSuggestion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"d":[{"id":"tt1375666","l":"Inception",
			"y":2010,"tl":"2010","qid":"movie",
			"i":{"imageUrl":"https://img.example/p.jpg"}}],"q":"tt1375666","v":1}`))
	}))
	defer server.Close()

	old := SuggestionAPIEndpoint
	SuggestionAPIEndpoint = server.URL + "/suggestion/%s/%s.json"
	defer func() { SuggestionAPIEndpoint = old }()

	detail, err := NewClient("").GetByIMDbID("tt1375666")
	if err != nil {
		t.Fatalf("GetByIMDbID 失败: %v", err)
	}
	if detail.Title != "Inception" || detail.Year != "2010" {
		t.Errorf("详情错误: %+v", detail)
	}
}

// TestGetByIDViaOfficialAPI 验证按 ID 查询详情
func TestGetByIDViaOfficialAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"data": {"title": {
				"id": "tt0816692",
				"titleText": {"text": "Interstellar"},
				"releaseYear": {"year": 2014},
				"titleType": {"text": "Movie"},
				"primaryImage": {"url": ""},
				"ratingsSummary": {"aggregateRating": 8.7},
				"plot": {"plainText": "A team of explorers travel beyond this galaxy."}
			}}
		}`))
	}))
	defer server.Close()

	old := OfficialAPIEndpoint
	OfficialAPIEndpoint = server.URL
	defer func() { OfficialAPIEndpoint = old }()

	detail, err := NewClient("key").GetByIMDbID("tt0816692")
	if err != nil {
		t.Fatalf("GetByIMDbID 失败: %v", err)
	}
	if detail.Title != "Interstellar" || detail.Year != "2014" || detail.IMDbID != "tt0816692" {
		t.Errorf("详情字段错误: %+v", detail)
	}
	if !strings.Contains(detail.Description, "explorers") {
		t.Errorf("简介未解析: %q", detail.Description)
	}
}
