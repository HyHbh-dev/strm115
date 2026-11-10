package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/find"
	"strm115/internal/linkparse"
	"strm115/internal/notify"
	"strm115/internal/pan"
)

// 设置键：IMDb 官方 API Key（留空则用免费接口）。
const settingIMDbKey = "imdb_api_key"

// handleFind 解析一段文本：认出链接、认出片名、顺带查出在哪能看。
//
// 这是个纯查询接口，不碰用户的网盘 —— 解析和「提交下载」分开，
// 用户可以先看清楚识别出来的是什么再决定。
func (s *Server) handleFind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, errors.New("先把要识别的内容粘进来"))
		return
	}
	// 一段文本再长也不该超过 64KB；大文件内容粘进来多半是误操作。
	if len(text) > 64<<10 {
		writeErr(w, http.StatusBadRequest, errors.New("内容太长了（超过 64KB）"))
		return
	}

	res, err := find.Analyze(r.Context(), text, find.Options{
		IMDbKey: s.setting(r.Context(), settingIMDbKey),
		Timeout: 40 * time.Second,
	})
	if err != nil {
		if errors.Is(err, find.ErrNoInput) {
			writeErr(w, http.StatusUnprocessableEntity, err)
			return
		}
		writeErr(w, statusOf(err), err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result":          res,
		"offline_enabled": s.offlineEnabled(r.Context()),
	})
}

// offlineEnabled 报告当前有没有「能离线下载」的账号，前端据此决定要不要显示按钮。
func (s *Server) offlineEnabled(ctx context.Context) bool {
	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		return false
	}
	for _, a := range accounts {
		ok, err := s.offline.Supports(ctx, a.ID)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// 离线提交里「用户没配好」的那几种情况。
//
// 用哨兵错误而不是字符串比较：机器人那边要把它们当成「配置问题」直接回给
// 用户，HTTP 这边要映射成 400（不是 502 —— 网关错误会让用户去查网络）。
var (
	errNoAccount    = errors.New("先添加一个 115 账号")
	errAccountGone  = errors.New("账号不存在")
	errNoOfflineURI = errors.New("没有要提交的链接")
)

// offlineStatusOf 把「配置没配好」映射成 400：这类错误是用户能自己改的，
// 返回 502 会让人去查网络，方向就错了。
func offlineStatusOf(err error) int {
	switch {
	case errors.Is(err, errNoAccount), errors.Is(err, errNoOfflineURI):
		return http.StatusBadRequest
	case errors.Is(err, errAccountGone):
		return http.StatusNotFound
	}
	return statusOf(err)
}

// submitOffline 把链接交给网盘离线下载，返回实际使用的账号与任务 id。
//
// 抽出来给 Telegram 机器人复用：离线提交的账号挑选、目录回退这些判断
// 只该有一份，否则「网页提交成功、机器人报没有账号」这种问题会反复出现。
func (s *Server) submitOffline(ctx context.Context, acctID int64, uris []string, saveCID, label string) (int64, []string, error) {
	if len(uris) == 0 {
		return 0, nil, errNoOfflineURI
	}
	// 没指定账号时挑第一个支持离线下载的：用户只有 115，没必要让他先选。
	if acctID == 0 {
		accounts, _ := s.st.ListAccounts(ctx)
		for _, a := range accounts {
			if ok, err := s.offline.Supports(ctx, a.ID); err == nil && ok {
				acctID = a.ID
				break
			}
		}
	}
	if acctID == 0 {
		return 0, nil, errNoAccount
	}
	if _, err := s.st.GetAccount(ctx, acctID); err != nil {
		return acctID, nil, errAccountGone
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	hashes, err := s.offline.Add(ctx, acctID, uris, strings.TrimSpace(saveCID), strings.TrimSpace(label))
	return acctID, hashes, err
}

// handleOfflineAdd 把链接提交给网盘离线下载。
func (s *Server) handleOfflineAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID int64    `json:"account_id"`
		URIs      []string `json:"uris"`
		SaveCID   string   `json:"save_cid"`
		Label     string   `json:"label"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	if len(body.URIs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("没有要提交的链接"))
		return
	}
	if len(body.URIs) > 20 {
		writeErr(w, http.StatusBadRequest, errors.New("一次最多提交 20 个链接"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	_, hashes, err := s.submitOffline(ctx, body.AccountID, body.URIs, body.SaveCID, body.Label)
	if err != nil {
		if len(hashes) > 0 {
			// 一部分进了队列、一部分被拒。直接报错会让用户以为全都没提交，
			// 所以把「已成功几条」写进错误信息里。
			writeErr(w, statusOf(err), fmt.Errorf("已提交 %d 条，其余失败：%w", len(hashes), err))
			return
		}
		writeErr(w, offlineStatusOf(err), err)
		return
	}
	// 提交阶段也发一条：离线要下几十分钟，用户关掉页面后不知道「到底交上去没有」。
	// 只推一条、只带片名，不重复「下完还会再推」这类说明。
	label := strings.TrimSpace(body.Label)
	if label == "" && len(body.URIs) > 0 {
		label = linkparse.CleanMediaName(body.URIs[0])
	}
	text := fmt.Sprintf("%d 个任务", len(hashes))
	if label != "" {
		text = label
	}
	if len(hashes) > 1 {
		text = fmt.Sprintf("%d 个任务 · %s", len(hashes), label)
	}
	s.pushEvent("offline_add", notify.KindTransfer, "⏳ 已提交离线", text)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"hashes": hashes,
		"count":  len(hashes),
	})
}

// handleOfflineTasks 列一页离线任务。
func (s *Server) handleOfflineTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	acct, _ := strconv.ParseInt(q.Get("account_id"), 10, 64)
	page, _ := strconv.ParseInt(q.Get("page"), 10, 64)
	if page <= 0 {
		page = 1
	}
	if acct == 0 {
		accounts, _ := s.st.ListAccounts(r.Context())
		for _, a := range accounts {
			if ok, err := s.offline.Supports(r.Context(), a.ID); err == nil && ok {
				acct = a.ID
				break
			}
		}
	}
	if acct == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"tasks": []pan.OfflineTask{}, "total": 0, "watching": 0,
			"error": "还没有支持离线下载的账号",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tasks, total, err := s.offline.Tasks(ctx, acct, page)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	if tasks == nil {
		tasks = []pan.OfflineTask{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":    tasks,
		"total":    total,
		"page":     page,
		"watching": s.offline.Watching(),
		"account":  acct,
	})
}
