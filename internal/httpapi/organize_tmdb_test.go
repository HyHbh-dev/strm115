package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"strm115/internal/organize"
	"strm115/internal/tmdb"
)

// 「搬之前先问 TMDB」是整个整理链路唯一会联网的一步：
// 它决定了「刮不到的片子是留在暂存目录，还是变成库里一张灰卡片」。
// 两个方向都要盯：认得出的必须放行，认不出的必须拦住。
func TestTMDbRecognized(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "")
	ctx := context.Background()

	// 假上游的 /search/movie 只回《沙丘2 / Dune: Part Two / 2024》，
	// 片名和年份都对得上，应当放行。
	ok, note := s.tmdbRecognized(ctx, organize.Item{
		Kind: organize.KindMovie, Title: "沙丘2", Year: "2024",
	})
	if !ok {
		t.Fatalf("认得出的片子不该被拦：%s", note)
	}

	// 片名完全对不上：留在暂存目录，并给出人话原因。
	ok, note = s.tmdbRecognized(ctx, organize.Item{
		Kind: organize.KindMovie, Title: "完全不相干的一部片子", Year: "1998",
	})
	if ok {
		t.Fatal("TMDB 里找不到的条目应被拦下")
	}
	if note == "" {
		t.Fatal("拦下时必须给原因，否则台账里只有一条空记录")
	}
}

// TestTMDbRecognizedEnglishOnlyName 盯住「发布名只有英文、TMDB 的 zh-CN 名是中文」。
//
// 《凡人修仙传》的真实发布名就是 `A Record of a Mortal’s Journey to Immortality`，
// 而 TMDB 的 zh-CN 名字只有「凡人修仙传」。只按 zh-CN 搜，片名对不上，
// 整理会把文件一直留在暂存目录（线上真实踩到的那一条）。这里要求：
// 第一次没认出来时，退一步用 en-US 再搜一次，那边返回的就是这个英文名。
func TestTMDbRecognizedEnglishOnlyName(t *testing.T) {
	s, st := newServerWithStore(t)
	oldAPI, oldImage := tmdb.CurrentUpstream()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/search/tv" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("language") == "en-US" {
			_, _ = io.WriteString(w, `{"results":[{"id":106449,
				"name":"A Record of a Mortal's Journey to Immortality",
				"original_name":"\u51e1\u4eba\u4fee\u4ed9\u4f20","first_air_date":"2020-07-25","popularity":42.8}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"id":106449,"name":"\u51e1\u4eba\u4fee\u4ed9\u4f20",
			"original_name":"\u51e1\u4eba\u4fee\u4ed9\u4f20","first_air_date":"2020-07-25","popularity":42.8}]}`)
	}))
	ctx := context.Background()
	if err := st.SetSetting(ctx, "tmdb_base", srv.URL); err != nil {
		t.Fatal(err)
	}
	s.applyTMDBUpstream(ctx)
	t.Cleanup(func() {
		tmdb.SetUpstream(oldAPI, oldImage)
		srv.Close()
	})

	ok, note := s.tmdbRecognized(ctx, organize.Item{
		Kind: organize.KindSeries, Title: "A Record of a Mortal\u2019s Journey to Immortality",
		Season: 1, Episode: 199,
	})
	if !ok {
		t.Fatalf("英文发布名在 en-US 下能对上，不该被拦：%s", note)
	}
}
