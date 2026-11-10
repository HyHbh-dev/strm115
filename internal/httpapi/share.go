package httpapi

// 115 分享转存。
//
// 「搜索 → 找到 → 存进自己的网盘」这条链路的最后一步：资源搜索里 TG 帖子的
// 115 分享已经被 internal/linkparse 认出了分享码与提取码，前端点一下就把
// 码 + 提取码送到这里，不必再点开 t.me 的帖子复制粘贴、也不用跳去 115 网页。
//
// 入参三选一：
//   - url：115 分享链接（115.com / 115cdn.com / anxia.com 都认，密码可写在 ?password=）
//   - share_code + password：前端已经拆好
//   - share_code：只有码，115 会在转存时要求补提取码（会如实把 115 的原话返回）
//
// 目标目录默认取设置项 res_save_cid（资源搜索页可选），没配就是根目录。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/linkparse"
	"strm115/internal/pan"
)

// 「这条链接不是能转存的东西」的两种情况。前端拿到 400 会提示用户改输入，
// 拿到 502 只会以为服务挂了。
var (
	errNotShareLink = errors.New("这不是一条 115 分享链接")
	errNoShareCode  = errors.New("没有识别到 115 分享码")
)

// shareInput 把请求里的三种写法（链接 / 码+提取码 / 只有码）收敛成「码 + 提取码」。
//
// 给了链接就以链接为准：里面的分享码比前端传的可靠 —— 前端有可能把整段文案
// 当 url 递上来，Parse115Share 也认；但反过来「前端说 swaaa、链接里是 swbbb」时，
// 用户点的那行结果对应的显然是链接里的码。
func shareInput(rawURL, code, pwd string) (string, string, error) {
	if raw := strings.TrimSpace(rawURL); raw != "" {
		sh := linkparse.Parse115Share(raw)
		if sh == nil {
			return "", "", errNotShareLink
		}
		code = sh.ShareCode
		if strings.TrimSpace(pwd) == "" {
			pwd = sh.Password
		}
	}
	code = strings.TrimSpace(code)
	pwd = strings.TrimSpace(pwd)
	if code == "" {
		return "", "", errNoShareCode
	}
	return code, pwd, nil
}

// shareSaveOut 是一次转存的结果，HTTP 接口和 Telegram 机器人共用。
type shareSaveOut struct {
	Title     string
	Files     int
	Already   bool
	CID       string
	Dir       string // 人类可读的目标位置（媒体库名 / 网盘目录名 / cid）
	AccountID int64
	Account   string // 账号名
}

// saveShareTo 把一条 115 分享转存到 acct 的网盘 cid 目录下。
//
// 抽出来是因为 Telegram 机器人要用同一份逻辑：转存只有一条正确路径，
// 两边各写一遍就会出现「网页能存、机器人报错」这种没人能解释的差异。
func (s *Server) saveShareTo(ctx context.Context, acctID int64, rawURL, code, pwd, cid string) (*shareSaveOut, error) {
	code, pwd, err := shareInput(rawURL, code, pwd)
	if err != nil {
		return nil, err
	}

	if acctID == 0 {
		accounts, err := s.st.ListAccounts(ctx)
		if err != nil {
			return nil, err
		}
		for _, a := range accounts {
			// 转存只有 115 能做；账号不多，直接挑第一个 115 的。
			if strings.EqualFold(a.Kind, "115") {
				acctID = a.ID
				break
			}
		}
	}
	if acctID == 0 {
		return nil, errNoAccount
	}

	if strings.TrimSpace(cid) == "" {
		cid = strings.TrimSpace(s.setting(ctx, "res_save_cid"))
	}
	if cid == "" {
		cid = "0"
	}

	mgr, err := s.pool.Get(ctx, acctID)
	if err != nil {
		return nil, err
	}
	if !mgr.SupportsShareSave() {
		return nil, errors.New("当前账号不支持分享转存")
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	res, err := mgr.SaveShare(ctx, code, pwd, cid)
	if err != nil {
		return nil, err
	}

	out := &shareSaveOut{
		Title: res.Title, Files: res.Files, Already: res.Already,
		CID: cid, AccountID: acctID,
	}
	out.Account = s.accountName(ctx, acctID)
	out.Dir = s.dirLabel(ctx, mgr, cid)
	return out, nil
}

// accountName 取账号名，取不到就退回 id。
func (s *Server) accountName(ctx context.Context, acctID int64) string {
	if a, err := s.st.GetAccount(ctx, acctID); err == nil && a.Name != "" {
		return a.Name
	}
	return "账号 " + strconv.FormatInt(acctID, 10)
}

// dirLabel 把 cid 翻译成人看得懂的位置。
//
// 优先认媒体库（「电影库」比一串 19 位数字有用得多）；
// 不是库根就查一次目录名 —— 转存已经花了几秒，多一次查询换一句
// 「目录：/电影/待整理」是值得的。查不到就退回 cid 本身，不报错。
func (s *Server) dirLabel(ctx context.Context, mgr *pan.Manager, cid string) string {
	if cid == "" || cid == "0" {
		return "根目录"
	}
	if libs, err := s.st.ListLibraries(ctx); err == nil {
		for _, l := range libs {
			if libHasDir(l, cid) {
				return "媒体库「" + l.Name + "」"
			}
		}
	}
	if e, err := mgr.Stat(ctx, cid); err == nil && e.Name != "" {
		return e.Name
	}
	return cid
}

// shareStatusOf 把「用户填错了」映射成 400，其余交给 statusOf。
func shareStatusOf(err error) int {
	switch {
	case errors.Is(err, errNotShareLink), errors.Is(err, errNoShareCode), errors.Is(err, errNoAccount):
		return http.StatusBadRequest
	}
	return statusOf(err)
}

// handleSaveShare 把一条 115 分享转存到自己的网盘。
func (s *Server) handleSaveShare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID int64  `json:"account_id"`
		URL       string `json:"url"`
		ShareCode string `json:"share_code"`
		Password  string `json:"password"`
		CID       string `json:"cid"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}

	out, err := s.saveShareTo(r.Context(), body.AccountID, body.URL, body.ShareCode, body.Password, body.CID)
	if err != nil {
		s.log.Warn("115 分享转存失败", "code", body.ShareCode, "err", err)
		writeErr(w, shareStatusOf(err), err)
		return
	}
	s.log.Info("115 分享已转存", "account", out.AccountID, "title", out.Title,
		"files", out.Files, "already", out.Already, "cid", out.CID)

	// 存进资源保存目录之后，后半段（整理 → 同步 → 刮削）自动接上；
	// 存到别处（比如直接选了某个媒体库目录）就交给那边自己的流程。
	if !out.Already && s.organizerIngestOn(r.Context()) {
		want := strings.TrimSpace(body.CID)
		save := strings.TrimSpace(s.setting(r.Context(), "res_save_cid"))
		if want == "" || want == save {
			s.kickOrganizeIngest(out.AccountID, 3*time.Second, "网页转存")
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"title":      out.Title,
		"files":      out.Files,
		"already":    out.Already,
		"cid":        out.CID,
		"dir":        out.Dir,
		"account_id": out.AccountID,
	})
}
