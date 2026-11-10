package tgbot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"strm115/internal/resource"
)

// subLog 记下机器人到底发了什么样的订阅请求。
type subLog struct {
	reqs []SubscribeRequest
	res  *SubscribeResult
	err  error
}

// subDeps 在测试脚手架上接一份「订阅能力」，其余依赖保持 recorder 的默认。
func subDeps(f *fakeTG, items []resource.Item, sl *subLog) Deps {
	d := (&recorder{}).deps(f, items)
	d.Subscribe = func(_ context.Context, req SubscribeRequest) (*SubscribeResult, error) {
		sl.reqs = append(sl.reqs, req)
		if sl.err != nil {
			return nil, sl.err
		}
		if sl.res != nil {
			return sl.res, nil
		}
		return &SubscribeResult{ID: 7, Title: req.Title}, nil
	}
	d.SubscribeSeasons = func(_ context.Context, _ int) ([]SubSeason, error) {
		return []SubSeason{
			{Season: 1, Episodes: 3, Name: "第一季"},
			{Season: 2, Episodes: 2, Name: "第二季"},
		}, nil
	}
	d.SearchTMDB = func(_ context.Context, _ string, _ int) ([]TitleHit, error) {
		return []TitleHit{
			{ID: 101, Title: "沙丘2", Original: "Dune: Part Two", Year: "2024", Kind: "movie"},
			{ID: 202, Title: "测试剧", Original: "Test Show", Year: "2023", Kind: "tv"},
		}, nil
	}
	d.TMDBName = func(_ context.Context, _ string, _ int) ([]TitleHit, error) {
		return []TitleHit{
			{ID: 101, Title: "Dune: Part Two", Original: "Dune: Part Two", Year: "2024", Kind: "movie"},
		}, nil
	}
	return d
}

func cb(f *fakeTG, data string, msgID int64) *CallbackQuery {
	return &CallbackQuery{
		ID: "cb", Data: data,
		Message: &Message{MessageID: msgID, Chat: Chat{ID: 1}},
	}
}

// flatCallbackData 把键盘里所有回调数据拼成一行，方便断言某个按钮在不在。
func flatCallbackData(kb InlineKeyboard) string {
	var b strings.Builder
	for _, row := range kb {
		for _, btn := range row {
			b.WriteString(btn.CallbackData)
			b.WriteString(" ")
		}
	}
	return b.String()
}

// subDepsWithSeasons 换掉季集数，好造「一季 79 集」「一季 130 集」这种长清单。
func subDepsWithSeasons(f *fakeTG, sl *subLog, seasons []SubSeason) Deps {
	d := subDeps(f, nil, sl)
	d.SubscribeSeasons = func(_ context.Context, _ int) ([]SubSeason, error) {
		return seasons, nil
	}
	return d
}

// 没接订阅能力时不该出现一个点了没反应的按钮；接上后每部候选同一行多一个订阅。
func TestSubscribeButtonOnlyWhenAvailable(t *testing.T) {
	for _, withSub := range []bool{false, true} {
		f := &fakeTG{}
		srv := f.server(t)
		sl := &subLog{}
		d := subDeps(f, nil, sl)
		if !withSub {
			d.Subscribe = nil
			d.SubscribeSeasons = nil
		}
		b := New(d)
		b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))

		sent, _ := f.last("sendMessage")
		kb := keyboardOf(t, sent)
		if len(kb) < 2 {
			t.Fatalf("候选键盘至少两行，实际 %v", kb)
		}
		if got := kb[0][0].CallbackData; got != "t:1" {
			t.Fatalf("选片按钮不能被挤掉：%q", got)
		}
		got := ""
		if len(kb[0]) > 1 {
			got = kb[0][1].CallbackData
		}
		if withSub && got != "sb:1" {
			t.Errorf("接了订阅应有 sb:1，实际 %q（%v）", got, kb[0])
		}
		if !withSub && got != "" {
			t.Errorf("没接订阅不该有第二个按钮，实际 %q", got)
		}
	}
}

// 电影点「⭐订阅」直接建好，没有选集步骤，也不带起播线。
func TestSubscribeMovieFromCandidates(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	sl := &subLog{}
	b := New(subDeps(f, nil, sl))
	api := f.api(srv)

	b.onMessage(context.Background(), api, chatMsg(1, "沙丘2"))
	b.onCallback(context.Background(), api, cb(f, "sb:1", f.lastSentID(t)))

	if len(sl.reqs) != 1 {
		t.Fatalf("电影应直接建 1 次订阅，实际 %v", sl.reqs)
	}
	req := sl.reqs[0]
	if req.TMDbID != 101 || req.Kind != "movie" || req.Title != "沙丘2" {
		t.Errorf("订阅请求不对：%+v", req)
	}
	if req.WatermarkSeason != 0 || req.WatermarkEpisode != 0 {
		t.Errorf("电影不该带起播线：%+v", req)
	}
	if !strings.Contains(anyText(f), "已订阅《沙丘2》）") &&
		!strings.Contains(anyText(f), "已订阅《沙丘2》") {
		t.Errorf("没有订阅成功回执：%s", anyText(f))
	}
}

// 剧集：选季 → 选集 → 建订阅，起播线要真的传下去，回执要显示日历集数。
func TestSubscribeTVChoosesStartEpisode(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	sl := &subLog{res: &SubscribeResult{ID: 9, Title: "测试剧", CalendarTotal: 12}}
	b := New(subDeps(f, nil, sl))
	api := f.api(srv)

	b.onMessage(context.Background(), api, chatMsg(1, "沙丘2"))
	b.onCallback(context.Background(), api, cb(f, "sb:2", f.lastSentID(t)))

	if len(sl.reqs) != 0 {
		t.Fatal("剧集不能直接建订阅，应先问起始集")
	}
	start, _ := f.last("sendMessage")
	if !strings.Contains(start.payload["text"].(string), "从哪一集开始订阅") {
		t.Fatalf("没发选季界面：%v", start.payload["text"])
	}
	kb := keyboardOf(t, start)
	if kb[0][0].CallbackData != "sbg" {
		t.Errorf("第一行应是「从头追」，实际 %v", kb[0])
	}
	flat := ""
	for _, row := range kb {
		for _, btn := range row {
			flat += btn.CallbackData + " "
		}
	}
	if !strings.Contains(flat, "sbs:1 ") || !strings.Contains(flat, "sbs:2 ") {
		t.Errorf("应有两季可选，实际 %s", flat)
	}

	// 选第 2 季：改写同一条消息成集号键盘。
	b.onCallback(context.Background(), api, cb(f, "sbs:2", start.msgID))
	edit, ok := f.last("editMessageText")
	if !ok {
		t.Fatal("选季应原地改写消息")
	}
	if !strings.Contains(edit.payload["text"].(string), "第 2 季") {
		t.Errorf("改写后应写明第 2 季：%v", edit.payload["text"])
	}
	ekb := keyboardOf(t, edit)
	epFlat := ""
	for _, row := range ekb {
		for _, btn := range row {
			epFlat += btn.CallbackData + " "
		}
	}
	if !strings.Contains(epFlat, "sbe:2:1 ") || !strings.Contains(epFlat, "sbback") {
		t.Errorf("集号键盘不对：%s", epFlat)
	}

	// 点 E02 → 建订阅，起播线 S02E02。
	b.onCallback(context.Background(), api, cb(f, "sbe:2:2", start.msgID))
	if len(sl.reqs) != 1 {
		t.Fatalf("选完集应只建 1 次订阅，实际 %v", sl.reqs)
	}
	req := sl.reqs[0]
	if req.Kind != "tv" || req.WatermarkSeason != 2 || req.WatermarkEpisode != 2 {
		t.Errorf("起播线没传对：%+v", req)
	}
	text := anyText(f)
	if !strings.Contains(text, "S02E02") || !strings.Contains(text, "12 集专有追更日历") {
		t.Errorf("回执该写起始集与日历集数：%s", text)
	}
}

// 「从头追」不该经过选集，也不该带起播线。
func TestSubscribeTVFromBeginning(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	sl := &subLog{}
	b := New(subDeps(f, nil, sl))
	api := f.api(srv)

	b.onMessage(context.Background(), api, chatMsg(1, "沙丘2"))
	b.onCallback(context.Background(), api, cb(f, "sb:2", f.lastSentID(t)))
	b.onCallback(context.Background(), api, cb(f, "sbg", f.lastSentID(t)))

	if len(sl.reqs) != 1 {
		t.Fatalf("从头追应建 1 次订阅，实际 %v", sl.reqs)
	}
	if sl.reqs[0].WatermarkSeason != 0 || sl.reqs[0].WatermarkEpisode != 0 {
		t.Errorf("从头追不该有起播线：%+v", sl.reqs[0])
	}
}

// /name 的英文名回复也带订阅按钮，点下去同样能建订阅。
func TestSubscribeFromNameLookup(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	sl := &subLog{}
	b := New(subDeps(f, nil, sl))
	api := f.api(srv)

	b.onMessage(context.Background(), api, chatMsg(1, "/name 沙丘2"))
	sent, _ := f.last("sendMessage")
	kb := keyboardOf(t, sent)
	if len(kb) != 1 || kb[0][0].CallbackData != "sbn:1" {
		t.Fatalf("/name 回复应带一个订阅按钮，实际 %v", kb)
	}
	b.onCallback(context.Background(), api, cb(f, "sbn:1", sent.msgID))
	if len(sl.reqs) != 1 || sl.reqs[0].TMDbID != 101 {
		t.Fatalf("英文名回复的订阅没建起来：%v", sl.reqs)
	}
}

// 认片 → 搜完资源之后，清单底部还有一个「订阅这部」。
func TestSubscribeFromResourceList(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	sl := &subLog{}
	d := subDeps(f, []resource.Item{{Title: "沙丘2 2160p", Magnet: "magnet:?xt=1"}}, sl)
	b := New(d)
	api := f.api(srv)

	b.onMessage(context.Background(), api, chatMsg(1, "沙丘2"))
	b.onCallback(context.Background(), api, cb(f, "t:1", f.lastSentID(t)))

	list, _ := f.last("sendMessage")
	kb := keyboardOf(t, list)
	last := kb[len(kb)-1]
	if last[0].CallbackData != "x" || len(last) < 2 || last[1].CallbackData != "sbp" {
		t.Fatalf("资源清单最后一行该有「订阅这部」，实际 %v", last)
	}
	b.onCallback(context.Background(), api, cb(f, "sbp", list.msgID))
	if len(sl.reqs) != 1 || sl.reqs[0].TMDbID != 101 {
		t.Fatalf("资源清单的订阅没建起来：%v", sl.reqs)
	}
}

// 79 集的季按 50 集分两段：第 0 段 E01–E50、第 1 段 E51–E79，
// 后面的集不再被截断，E79 能真正点进订阅。
func TestSubscribeEpisodeSegments79(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	sl := &subLog{}
	b := New(subDepsWithSeasons(f, sl, []SubSeason{{Season: 1, Episodes: 79, Name: "第一季"}}))
	api := f.api(srv)

	b.onMessage(context.Background(), api, chatMsg(1, "沙丘2"))
	b.onCallback(context.Background(), api, cb(f, "sb:2", f.lastSentID(t)))
	seasonMsg, _ := f.last("sendMessage")
	msgID := seasonMsg.msgID

	// 选季 → 第 0 段
	b.onCallback(context.Background(), api, cb(f, "sbs:1", msgID))
	page0, ok := f.last("editMessageText")
	if !ok {
		t.Fatal("选季应原地改写成集号键盘")
	}
	text0, _ := page0.payload["text"].(string)
	for _, want := range []string{"共 79 集", "第 1/2 段", "E01–E50"} {
		if !strings.Contains(text0, want) {
			t.Errorf("第 0 段文案缺少 %q：%s", want, text0)
		}
	}
	if strings.Contains(text0, "只列出") {
		t.Errorf("不该再出现旧版的截断提示：%s", text0)
	}
	flat0 := flatCallbackData(keyboardOf(t, page0))
	for _, want := range []string{"sbe:1:1 ", "sbe:1:50 ", "sbpg:1 ", "sbback"} {
		if !strings.Contains(flat0, want) {
			t.Errorf("第 0 段键盘缺少 %q：%s", want, flat0)
		}
	}
	if strings.Contains(flat0, "sbe:1:51 ") {
		t.Errorf("第 0 段不该渲染 E51：%s", flat0)
	}

	// sbpg 翻到第 2 段（page=1）
	b.onCallback(context.Background(), api, cb(f, "sbpg:1", msgID))
	page1, ok := f.last("editMessageText")
	if !ok {
		t.Fatal("翻段应原地改写同一条消息")
	}
	text1, _ := page1.payload["text"].(string)
	for _, want := range []string{"第 2/2 段", "E51–E79"} {
		if !strings.Contains(text1, want) {
			t.Errorf("第 1 段文案缺少 %q：%s", want, text1)
		}
	}
	flat1 := flatCallbackData(keyboardOf(t, page1))
	for _, want := range []string{"sbe:1:51 ", "sbe:1:79 ", "sbpg:0 "} {
		if !strings.Contains(flat1, want) {
			t.Errorf("第 1 段键盘缺少 %q：%s", want, flat1)
		}
	}
	if strings.Contains(flat1, "sbe:1:50 ") || strings.Contains(flat1, "sbpg:2 ") {
		t.Errorf("第 1 段不该有 E50 或下一段：%s", flat1)
	}

	// E79 能选：起播线原样进订阅请求（不再被 60 集上限截掉）
	b.onCallback(context.Background(), api, cb(f, "sbe:1:79", msgID))
	if len(sl.reqs) != 1 {
		t.Fatalf("选完 E79 应建 1 次订阅，实际 %v", sl.reqs)
	}
	if sl.reqs[0].WatermarkSeason != 1 || sl.reqs[0].WatermarkEpisode != 79 {
		t.Errorf("E79 起播线没传对：%+v", sl.reqs[0])
	}
}

// 分段边界：130 集能翻到第三段，越界翻段被挡住；50 集以内保持原键盘形状。
func TestSubscribeEpisodeSegmentEdges(t *testing.T) {
	t.Run("130 集三段与越界", func(t *testing.T) {
		f := &fakeTG{}
		srv := f.server(t)
		sl := &subLog{}
		b := New(subDepsWithSeasons(f, sl, []SubSeason{{Season: 1, Episodes: 130}}))
		api := f.api(srv)

		b.onMessage(context.Background(), api, chatMsg(1, "沙丘2"))
		b.onCallback(context.Background(), api, cb(f, "sb:2", f.lastSentID(t)))
		msgID := f.lastSentID(t)
		b.onCallback(context.Background(), api, cb(f, "sbs:1", msgID))

		// 直接翻到第三段（page=2）：E101–E130，还留着回第二段的按钮
		b.onCallback(context.Background(), api, cb(f, "sbpg:2", msgID))
		last, _ := f.last("editMessageText")
		flat := flatCallbackData(keyboardOf(t, last))
		for _, want := range []string{"sbe:1:101 ", "sbe:1:130 ", "sbpg:1 "} {
			if !strings.Contains(flat, want) {
				t.Errorf("第三段键盘缺少 %q：%s", want, flat)
			}
		}
		if strings.Contains(flat, "sbpg:3 ") {
			t.Errorf("最后一段不该还有下一段：%s", flat)
		}

		// 越界翻段：只回一句提示，不改写消息也不建订阅
		editsBefore := f.count("editMessageText")
		b.onCallback(context.Background(), api, cb(f, "sbpg:9", msgID))
		if f.count("editMessageText") != editsBefore {
			t.Errorf("越界翻段不该改写消息")
		}
		ans, ok := f.last("answerCallbackQuery")
		if !ok {
			t.Fatal("越界也要 answer，否则按钮一直转圈")
		}
		if txt, _ := ans.payload["text"].(string); !strings.Contains(txt, "没有这一段") {
			t.Errorf("越界应提示「没有这一段」：%v", ans.payload)
		}
		if len(sl.reqs) != 0 {
			t.Errorf("翻段不该建订阅：%v", sl.reqs)
		}
	})

	t.Run("50 集以内不显示翻段行", func(t *testing.T) {
		f := &fakeTG{}
		srv := f.server(t)
		sl := &subLog{}
		b := New(subDepsWithSeasons(f, sl, []SubSeason{{Season: 1, Episodes: 50}}))
		api := f.api(srv)

		b.onMessage(context.Background(), api, chatMsg(1, "沙丘2"))
		b.onCallback(context.Background(), api, cb(f, "sb:2", f.lastSentID(t)))
		msgID := f.lastSentID(t)
		b.onCallback(context.Background(), api, cb(f, "sbs:1", msgID))

		edit, _ := f.last("editMessageText")
		flat := flatCallbackData(keyboardOf(t, edit))
		if strings.Contains(flat, "sbpg:") {
			t.Errorf("只有一段时不该出现翻段按钮：%s", flat)
		}
		for _, want := range []string{"sbe:1:50 ", "sbback", "sbx"} {
			if !strings.Contains(flat, want) {
				t.Errorf("短季要保持原来的键盘形状，缺 %q：%s", want, flat)
			}
		}
	})
}

// 重复订阅和失败都要有像样的回执，不能只留一个转圈的按钮。
func TestSubscribeAlreadyAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		res        *SubscribeResult
		err        error
	}{
		{"already", "已经在订阅里了", &SubscribeResult{Already: true}, nil},
		{"failed", "失败：TMDB 挂了", nil, errors.New("TMDB 挂了")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTG{}
			srv := f.server(t)
			sl := &subLog{res: tc.res, err: tc.err}
			b := New(subDeps(f, nil, sl))
			api := f.api(srv)

			b.onMessage(context.Background(), api, chatMsg(1, "沙丘2"))
			b.onCallback(context.Background(), api, cb(f, "sb:1", f.lastSentID(t)))
			if !strings.Contains(anyText(f), tc.want) {
				t.Errorf("回执里应有「%s」：%s", tc.want, anyText(f))
			}
		})
	}
}
