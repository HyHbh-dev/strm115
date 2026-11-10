package httpapi

import (
	"os"
	"testing"
)

// 源码不内置 TMDB Key，兜底值来自 STRM115_TMDB_KEY。
//
// 这个包里有一批用 httptest 假上游的单测（海报墙、订阅、认片…）。没有 Key 时
// tmdb.Client 会以 ErrNoKey 直接短路，请求根本发不到假服务器，断言全崩。
// 在这里注入一把占位 Key 就够了：各测试仍会把上游指向本地 httptest，
// 不会真的去打 api.themoviedb.org。
func TestMain(m *testing.M) {
	_ = os.Setenv("STRM115_TMDB_KEY", "test-key")
	os.Exit(m.Run())
}
