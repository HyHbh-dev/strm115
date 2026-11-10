package httpapi

import (
	"strings"
	"testing"
)

// 自动整理没搬动任何东西时不推：每 5 分钟一条「无事发生」会让人直接静音。
func TestOrganizeNoticeAutoStaysSilent(t *testing.T) {
	if _, _, ok := organizeNotice(organizeRunResult{Skipped: 3}, false); ok {
		t.Fatal("自动整理没有搬动也没失败时不该推送")
	}
}

// 网页上手动点的那一轮必须回一条，哪怕是「没东西可搬」。
// 否则用户分不清「跑了但没东西」和「根本没跑」，只能去翻日志。
func TestOrganizeNoticeManualAlwaysReplies(t *testing.T) {
	title, line, ok := organizeNotice(organizeRunResult{Skipped: 2}, true)
	if !ok {
		t.Fatal("手动整理应回一条通知")
	}
	if !strings.Contains(title, "没有需要搬") {
		t.Fatalf("标题应说明没有需要搬的条目，实际 %q", title)
	}
	if !strings.Contains(line, "跳过 2") {
		t.Fatalf("正文应带上跳过条数，实际 %q", line)
	}
}

// 待整理目录整个是空的：正文要说人话，不能只给一排 0。
func TestOrganizeNoticeEmptySource(t *testing.T) {
	_, line, ok := organizeNotice(organizeRunResult{}, true)
	if !ok {
		t.Fatal("手动整理应回一条通知")
	}
	if !strings.Contains(line, "空的") {
		t.Fatalf("空目录时正文应直说，实际 %q", line)
	}
}

// 有片名时正文带上片名（最多三部），让用户一眼看到搬了什么。
func TestOrganizeNoticeListsNames(t *testing.T) {
	res := organizeRunResult{
		Moved:  5,
		Names:  []string{"F1：狂飙飞车", "奥德赛", "坠落2", "第四部", "第五部"},
		Synced: []string{"电影"},
	}
	_, line, ok := organizeNotice(res, false)
	if !ok {
		t.Fatal("搬动了就应该推送")
	}
	for _, want := range []string{"F1：狂飙飞车", "奥德赛", "坠落2", "等 5 部", "已同步 电影"} {
		if !strings.Contains(line, want) {
			t.Fatalf("正文缺少 %q：%q", want, line)
		}
	}
	if strings.Contains(line, "第四部") {
		t.Fatalf("只应列出前三部：%q", line)
	}
}

// 失败也算「有事发生」，自动整理要推。
func TestOrganizeNoticeAutoPushesOnFailure(t *testing.T) {
	if _, _, ok := organizeNotice(organizeRunResult{Failed: 1}, false); !ok {
		t.Fatal("有失败条目时自动整理应推送")
	}
}
