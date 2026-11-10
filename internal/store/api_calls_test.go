package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newAPICallStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestAPICallsRoundTrip 覆盖口径：同一天同一账号是**覆盖**写入（内存里本来就是
// 当日累计值），不同账号、不同天互不影响。
func TestAPICallsRoundTrip(t *testing.T) {
	st := newAPICallStore(t)
	ctx := context.Background()

	if err := st.SetAPICalls(ctx, "2026-11-08", 1, 12); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAPICalls(ctx, "2026-11-08", 1, 40); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAPICalls(ctx, "2026-11-08", 2, 7); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAPICalls(ctx, "2026-11-07", 1, 999); err != nil {
		t.Fatal(err)
	}

	got, err := st.APICallsOn(ctx, "2026-11-08")
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != 40 {
		t.Fatalf("同一天重复写应当覆盖而不是累加，实际 %d", got[1])
	}
	if got[2] != 7 {
		t.Fatalf("另一个账号的数被串了: %d", got[2])
	}

	y, err := st.APICallsOn(ctx, "2026-11-07")
	if err != nil {
		t.Fatal(err)
	}
	if y[1] != 999 {
		t.Fatalf("前一天的账不该被今天的写坏: %d", y[1])
	}

	// 非法入参直接忽略，不能写出一行 0 号账号的垃圾
	if err := st.SetAPICalls(ctx, "2026-11-08", 0, 5); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAPICalls(ctx, "", 1, 5); err != nil {
		t.Fatal(err)
	}
	after, _ := st.APICallsOn(ctx, "2026-11-08")
	if _, ok := after[0]; ok {
		t.Fatal("账号 0 不该落库")
	}
}

func TestPruneAPICalls(t *testing.T) {
	st := newAPICallStore(t)
	ctx := context.Background()
	for _, d := range []string{"2026-10-01", "2026-11-07", "2026-11-08"} {
		if err := st.SetAPICalls(ctx, d, 1, 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PruneAPICalls(ctx, "2026-11-01"); err != nil {
		t.Fatal(err)
	}
	if old, _ := st.APICallsOn(ctx, "2026-10-01"); len(old) != 0 {
		t.Fatalf("一个月前的账应当被剪掉: %v", old)
	}
	if keep, _ := st.APICallsOn(ctx, "2026-11-08"); keep[1] != 3 {
		t.Fatalf("保留期内的账被误删: %v", keep)
	}
}
