package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"strm115/internal/notify"
)

// 通知相关的设置键。
const (
	settingNotifyConfig    = "notify_config"
	settingNotifyEvents    = "notify_events"
	settingOfflineAutoSync = "offline_auto_sync"
	// settingNotifyEventsVersion 标记「这份订阅清单是哪个版本存的」。
	// 只用来判断要不要把新事件补进去，值本身没有业务含义。
	settingNotifyEventsVersion = "notify_events_version"
	notifyEventsVersion        = "2"
)

// notifyEventLabels 事件清单。json tag 不能省：前端按 id/label 渲染勾选框，
// 少了 tag 会变成 "ID"/"Label"，前端拿到 undefined —— 勾选框变成空白，
// 保存时再把 events 传成空数组，等于静默把所有事件关掉。
//
// 文案一律「一行标题 + 一行数字」：手机上一条消息只值一眼，塞满五行的推送
// 会被直接划掉。顺序按一条片子的生命周期排：提交 → 下载 → 整理 → 同步 →
// 刮削 → 入库 → 字幕 → 删除。
var notifyEventLabels = []struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
}{
	{"offline_add", "离线提交", "磁力 / 种子交给 115 离线下载时推一条"},
	{"offline_ok", "离线下载完成", "离线任务下完后推，配了自动同步会带上同步结果"},
	{"offline_fail", "离线下载失败", "链接失效 / 没有可用节点时推"},
	{"organize_ok", "自动整理完成", "整理执行完推一条：移入几部、跳过几部、失败几部"},
	{"sync_ok", "同步完成", "每次全量 / 增量同步跑完后推：目录、视频、写入、清理、失败"},
	{"sync_fail", "同步失败", "同步中断或 115 报错，带上失败原因"},
	{"scrape_ok", "刮削完成", "整库刮削跑完推一条：命中几部、未匹配几部"},
	{"ingest", "新片入库", "刮削出新条目时推带海报的入库卡片（一次最多 3 条，其余合并）"},
	{"subtitle_ok", "字幕补齐", "整库补字幕跑完推一条：新增、翻译、无源、失败"},
	{"delete", "删除", "从海报墙或网盘删片时推一条（本地索引 / 网盘源文件 / Emby 条目）"},
}

// notifySecretPaths 是配置里不允许回传给浏览器的字段。
//
// 用「JSON 路径」而不是给每个结构体写一个脱敏方法：渠道有八个，字段还会随
// 上游增加，维护一张表比维护八个函数可靠。空值表示「保留原值」，所以前端
// 留空保存不会把已配好的密钥冲掉。
var notifySecretPaths = [][]string{
	{"wecom", "secret"},
	{"wecom", "token"},
	{"wecom", "encoding_aes_key"},
	{"dingtalk", "secret"},
	{"tg", "token"},
	{"feishu", "secret"},
	{"qq_onebot", "token"},
	{"qq_official", "secret"},
	{"iyuu", "token"},
}

// loadNotifyConfig 读设置里的渠道配置。解析失败就当没配（不阻断启动）。
func (s *Server) loadNotifyConfig(ctx context.Context) notify.Config {
	raw, ok := s.st.GetSetting(ctx, settingNotifyConfig)
	if !ok || strings.TrimSpace(raw) == "" {
		return s.withNetProxy(ctx, notify.Config{})
	}
	var cfg notify.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		s.log.Warn("通知配置解析失败，按未配置处理", "err", err)
		return s.withNetProxy(ctx, notify.Config{})
	}
	return s.withNetProxy(ctx, cfg)
}

// withNetProxy 让「外推通知」也走设置里的网络代理。
//
// 这条以前漏了：Telegram 机器人（长轮询）用的是 net_proxy，而通知推送用的是
// 通知配置里另一个 proxy_url —— 用户只填了 net_proxy，于是机器人连得上、
// 「发送测试」却一直 timeout，日志里只有一句 context deadline exceeded。
// 两处读同一个代理，行为才不会互相打架。
//
// 只在「通知自己没填代理」时兜底，不覆盖用户显式给通知配的代理。
func (s *Server) withNetProxy(ctx context.Context, cfg notify.Config) notify.Config {
	if strings.TrimSpace(cfg.ProxyURL) == "" {
		cfg.ProxyURL = s.netProxy(ctx)
	}
	return cfg
}

// reloadNotify 把设置里的配置推给常驻客户端。
func (s *Server) reloadNotify(ctx context.Context) {
	s.notify.Reload(s.loadNotifyConfig(ctx))
}

// sendMessage 是唯一的扇出点。永远异步、永远不返回错误：通知失败绝不能
// 让同步或下载流程跟着失败。
func (s *Server) sendMessage(m notify.Message) {
	if len(s.notify.Channels()) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		results := s.notify.SendRich(ctx, m)
		for _, r := range results {
			if !r.OK && !r.Skipped {
				s.log.Warn("通知发送失败", "channel", r.Channel, "err", r.Error)
			}
		}
	}()
}

// pushEvent 按「是否订阅该事件」决定要不要发。
func (s *Server) pushEvent(event string, kind notify.Kind, title, content string) {
	m := notify.Message{Kind: kind, Title: title, Content: content}
	// 关掉外推只是不推送,站内仍要留记录,所以放在 eventOn 判断之前。
	s.recordNotification(event, m)
	if !s.eventOn(event) {
		return
	}
	s.sendMessage(m)
}

// pushEventImage 带封面的推送：imageFile 是本地图片路径，交给渠道自己决定
// 能不能发（Telegram 直传文件，其它渠道退纯文本）。
func (s *Server) pushEventImage(event string, kind notify.Kind, title, content, imageFile string) {
	m := notify.Message{Kind: kind, Title: title, Content: content, ImageFile: imageFile}
	s.recordNotification(event, m)
	if !s.eventOn(event) {
		return
	}
	s.sendMessage(m)
}

// eventOn 判断某个事件是否被订阅。
//
// 「从没配过」和「全都取消勾选」必须区分开：前者全开（省得用户一进来就收不到
// 任何东西），后者全关（用户明确表达了不要）。
func (s *Server) eventOn(event string) bool {
	raw, ok := s.st.GetSetting(context.Background(), settingNotifyEvents)
	if !ok {
		return true
	}
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == event {
			return true
		}
	}
	return false
}

// notifyEventsSinceV2 是第二批加进来的事件（原先只有同步 / 离线四条）。
var notifyEventsSinceV2 = []string{
	"offline_add", "organize_ok", "scrape_ok", "ingest", "subtitle_ok", "delete",
}

// migrateNotifyEvents 把新增事件补进「已经存过订阅」的配置。
//
// 不做这一步会出现一个很难自证的状况：老用户升级后，界面上十个事件里六个是
// 关的，而他从来没关过 —— 分不清是「升级后默认关」还是「自己手滑关了」，
// 最后只能猜。所以按「旧清单存档」判定一次：只要 `notify_events` 存在、
// 又没有版本标记，就认为这份清单是上一版存的，把新事件补上并打标记。
//
// 例外：存的是空字符串（用户明确全关）时不补。空 = 全关，这个语义不能破坏。
func (s *Server) migrateNotifyEvents(ctx context.Context) {
	raw, ok := s.st.GetSetting(ctx, settingNotifyEvents)
	if !ok {
		return // 从没配过 = 全开，新事件天然生效
	}
	if _, done := s.st.GetSetting(ctx, settingNotifyEventsVersion); done {
		return
	}
	if strings.TrimSpace(raw) == "" {
		_ = s.st.SetSetting(ctx, settingNotifyEventsVersion, notifyEventsVersion)
		return
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(notifyEventsSinceV2)+4)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	added := 0
	for _, id := range notifyEventsSinceV2 {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
			added++
		}
	}
	if err := s.st.SetSetting(ctx, settingNotifyEvents, strings.Join(out, ",")); err != nil {
		s.log.Warn("补写通知事件失败", "err", err)
		return
	}
	_ = s.st.SetSetting(ctx, settingNotifyEventsVersion, notifyEventsVersion)
	if added > 0 {
		s.log.Info("通知事件已补齐（新增事件默认开启，可在消息配置里取消）", "added", added)
	}
}

// ==================== 接口 ====================

// handleGetNotify 返回打码后的配置。明文密钥不出服务端。
func (s *Server) handleGetNotify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.loadNotifyConfig(ctx)

	b, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var masked map[string]any
	if err := json.Unmarshal(b, &masked); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	secretSet := map[string]bool{}
	for _, p := range notifySecretPaths {
		if jsonString(masked, p) != "" {
			secretSet[strings.Join(p, ".")] = true
			jsonSet(masked, p, "")
		}
	}

	events := []string{}
	if raw, ok := s.st.GetSetting(ctx, settingNotifyEvents); ok {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				events = append(events, p)
			}
		}
	} else {
		for _, e := range notifyEventLabels {
			events = append(events, e.ID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"config":       masked,
		"secret_set":   secretSet,
		"channels":     s.notify.Channels(),
		"registered":   notify.RegisteredChannels(),
		"events":       events,
		"event_labels": notifyEventLabels,
		"auto_sync":    s.settingBool(ctx, settingOfflineAutoSync, true),
	})
}

// handlePutNotify 保存渠道配置。空密钥 = 保留原值。
func (s *Server) handlePutNotify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config map[string]any `json:"config"`
		// 用指针区分「没传」和「传了空数组」：没传就不动事件开关，
		// 传空数组才是「全关」。少这一层，客户端漏一个字段就会静默关掉所有通知。
		Events   *[]string `json:"events"`
		AutoSync *bool     `json:"auto_sync"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}

	ctx := r.Context()
	oldRaw, _ := s.st.GetSetting(ctx, settingNotifyConfig)
	var old map[string]any
	if strings.TrimSpace(oldRaw) != "" {
		_ = json.Unmarshal([]byte(oldRaw), &old)
	}

	merged := body.Config
	if merged == nil {
		merged = map[string]any{}
	}
	// 前端没回传的密钥沿用旧值。
	mergeNotifySecrets(merged, old)

	b, err := json.Marshal(merged)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var cfg notify.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("配置格式不对："+err.Error()))
		return
	}
	// 只保留合法的事件名，避免脏数据让 eventOn 永远为假。
	seen := map[string]bool{}
	clean := make([]string, 0, 4)
	if body.Events != nil {
		for _, e := range *body.Events {
			for _, known := range notifyEventLabels {
				if e == known.ID && !seen[e] {
					seen[e] = true
					clean = append(clean, e)
				}
			}
		}
	}

	if err := s.st.SetSetting(ctx, settingNotifyConfig, string(b)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if body.Events != nil {
		if err := s.st.SetSetting(ctx, settingNotifyEvents, strings.Join(clean, ",")); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if body.AutoSync != nil {
		v := "false"
		if *body.AutoSync {
			v = "true"
		}
		if err := s.st.SetSetting(ctx, settingOfflineAutoSync, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.reloadNotify(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"channels": s.notify.Channels(),
	})
}

// handleTestNotify 发一条测试消息。传了 channel 就只发给那一个渠道，
// 不传则扇出到所有启用渠道（保持旧行为，脚本调用不受影响）。
func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel string `json:"channel"`
	}
	// 请求体可以没有：老前端和 curl 都不带 body。
	_ = readJSON(r, &body)
	channel := strings.TrimSpace(body.Channel)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	results := s.notify.Test(ctx)
	if channel != "" {
		results = s.notify.TestChannel(ctx, channel)
	}
	sent, failed := 0, 0
	for _, x := range results {
		if x.OK {
			sent++
		} else if !x.Skipped {
			failed++
		}
	}
	msg := "没有启用的渠道，先去上面勾选一个"
	if channel != "" && len(results) == 0 {
		msg = channel + " 还没启用（或配置不完整），先保存再测"
	} else if sent > 0 && failed == 0 {
		msg = "已发送，去群里看看"
	} else if sent > 0 {
		msg = "部分渠道失败"
	} else if len(results) > 0 {
		msg = "全部失败，看下面的原因"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      failed == 0 && sent > 0,
		"message": msg,
		"channel": channel,
		"results": results,
	})
}

// handleTGDiscover 用 Bot Token 找出可用的 Chat ID。
//
// 只传 Token 是配置 Telegram 时最自然的起点（用户手上通常只有 Token），
// Chat ID 得靠 getUpdates 反查。这里替用户翻一遍 JSON，顺便确认 Token 有效。
func (s *Server) handleTGDiscover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	_ = readJSON(r, &body)
	token := strings.TrimSpace(body.Token)
	if token == "" {
		token = s.loadNotifyConfig(r.Context()).TG.Token
	}
	if token == "" {
		writeErr(w, http.StatusBadRequest, errors.New("先填 Bot Token 再点获取"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	info, err := s.notify.DiscoverTelegram(ctx, token)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// ==================== JSON 路径小工具 ====================

// mergeNotifySecrets 把前端没回传（空字符串）的密钥用旧值补上。
// 这是「留空 = 不修改」这条约定的唯一实现点。
func mergeNotifySecrets(in, old map[string]any) {
	for _, p := range notifySecretPaths {
		if jsonString(in, p) != "" {
			continue
		}
		if v := jsonString(old, p); v != "" {
			jsonSet(in, p, v)
		}
	}
}

func jsonString(m map[string]any, path []string) string {
	var cur any = m
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[k]
	}
	s, _ := cur.(string)
	return strings.TrimSpace(s)
}

func jsonSet(m map[string]any, path []string, v string) {
	if len(path) == 0 {
		return
	}
	cur := m
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
}
