package httpapi

import (
	"context"
	"testing"

	"strm115/internal/organize"
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
