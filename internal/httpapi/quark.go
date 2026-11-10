package httpapi

// 夸克这条链路有两个账号面：
//
//  1. TV 开放接口（refresh_token）：负责列目录、取 302 直链 —— 播放走它，
//     不用会员、不经服务端中转。扫码登录拿的就是它。
//  2. 网页接口（Cookie）：负责把别人的分享转存进自己的网盘 —— 只有它有
//     分享转存的能力，TV 接口没有。
//
// 两个凭证都放在同一个账号的 credential JSON 里（refresh_token / device_id /
// web_cookie），因为对用户来说那就是「一个夸克账号」。代价是这两样会分别失效：
// TV token 会自动刷新；网页 Cookie 大约 3 小时过期，过期时转存会明确报错，
// 界面上给一键重贴的入口（POST /api/accounts/{id}/quark-cookie）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"strm115/internal/pan/driverquarktv"
	"strm115/internal/quark"
	"strm115/internal/store"
)

const quarkKind = driverquarktv.Kind

// 这两个是本功能最常见的「还没配」状态，前端要能认出来提示去配。
var (
	errNoQuarkAccount = errors.New("还没有夸克账号：先去「账号」页扫码登录一个夸克TV 账号")
	errNoQuarkCookie  = errors.New("这个夸克账号还没配网页 Cookie：转存分享需要它（扫码登录只管播放）")
)

// quarkSession 是一次「扫码登录夸克TV」的会话。
type quarkSession struct {
	login *driverquarktv.QRLogin
	born  time.Time
	done  bool
	acct  *store.Account
}

// ==================== 扫码登录（TV 接口） ====================

// handleQuarkQRStart POST /api/accounts/quarktv/qr
//
// 返回的 png 是 data:image/jpeg;base64,… —— 夸克给的是 JPEG，
// 前缀写成 PNG 浏览器会直接裂开。
func (s *Server) handleQuarkQRStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	q := driverquarktv.NewQRLogin("", "")
	qr, err := q.Authorize(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	id := randToken()
	s.qkMu.Lock()
	s.qk[id] = &quarkSession{login: q, born: time.Now()}
	s.qkMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"png":        "data:image/jpeg;base64," + qr,
		"expires_in": 180,
	})
}

// handleQuarkQRPoll GET /api/accounts/quarktv/qr/{id}
//
// 没确认时返回 status=waiting（前端当长轮询用，不该弹红条）；
// 确认之后当场建账号并把凭证落库。
func (s *Server) handleQuarkQRPoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.qkMu.Lock()
	sess := s.qk[id]
	s.qkMu.Unlock()
	if sess == nil {
		writeErr(w, http.StatusNotFound, errors.New("二维码会话不存在或已过期"))
		return
	}
	if time.Since(sess.born) > 3*time.Minute {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "msg": "二维码已过期，请刷新"})
		return
	}
	if sess.done {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "account": sess.acct})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	cr, done, err := sess.login.Poll(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"status": "waiting"})
		return
	}

	name := strings.TrimSpace(sess.login.Nickname(ctx))
	if name == "" {
		name = "夸克TV"
	}
	raw, _ := json.Marshal(cr)
	acct, err := s.saveAccount(ctx, quarkKind, name, string(raw))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sess.done = true
	sess.acct = &acct
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "account": acct})
}

// ==================== 网页 Cookie（转存用） ====================

// handleQuarkCookie POST /api/accounts/{id}/quark-cookie  {cookie}
//
// 先拿 Cookie 真打一次探测再落库：贴错的 Cookie 存进去，只会让下一次转存
// 失败得更晚、更难查。
func (s *Server) handleQuarkCookie(w http.ResponseWriter, r *http.Request) {
	acctID := pathID(r, "id")
	if acctID == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("非法账号 id"))
		return
	}
	var body struct {
		Cookie string `json:"cookie"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	cookie := strings.TrimSpace(body.Cookie)
	if cookie == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请粘贴夸克网页 Cookie"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.saveQuarkCookie(ctx, acctID, cookie); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cookie_set": true})
}

// saveQuarkCookie 校验 Cookie → 写回账号 → 立刻点亮保活。
// 手动粘贴和扫码登录两条入口共用，行为必须一致。
func (s *Server) saveQuarkCookie(ctx context.Context, acctID int64, cookie string) error {
	c := quark.NewWithBase(cookie, strings.TrimSpace(s.setting(ctx, "quark_web_base")))
	if err := c.Ping(ctx); err != nil {
		return err
	}
	cr, err := s.quarkCredential(ctx, acctID)
	if err != nil {
		return err
	}
	cr.WebCookie = cookie
	raw, _ := json.Marshal(cr)
	if err := s.st.UpdateAccountCredential(ctx, acctID, string(raw)); err != nil {
		return err
	}
	if s.quarkKeep != nil {
		s.quarkKeep.poke()
	}
	return nil
}

// handleQuarkTest POST /api/quark/test {account_id}
//
// 两条通道分开报：TV（播放）挂了但 Cookie（转存）还行，用户应该知道
// 现在到底还能做什么。
func (s *Server) handleQuarkTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID int64 `json:"account_id"`
	}
	_ = readJSON(r, &body)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	acctID, cr, err := s.pickQuarkAccount(ctx, body.AccountID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out := map[string]any{"ok": true, "account_id": acctID, "cookie_set": cr.WebCookie != ""}
	// 以前的「测试」只 Ping 一下就报 ✓，用户却说「无法获取夸克网盘数据」——
	// Ping 通不代表列得出目录、读得到容量。这里把三件真事都做一遍，
	// 每一项单独回报，谁挂了一眼就能看出来。
	if mgr, err := s.pool.Get(ctx, acctID); err != nil {
		out["ok"] = false
		out["tv_error"] = err.Error()
	} else if err := mgr.Ping(ctx); err != nil {
		out["ok"] = false
		out["tv_error"] = err.Error()
	} else {
		out["tv_ok"] = true
		if entries, err := mgr.ListAll(ctx, "0", 50); err != nil {
			out["ok"] = false
			out["tv_list_error"] = err.Error()
		} else {
			out["tv_list_ok"] = true
			out["tv_files"] = len(entries)
		}
	}
	if cr.WebCookie == "" {
		out["web_error"] = errNoQuarkCookie.Error()
	} else {
		c := quark.NewWithBase(cr.WebCookie, strings.TrimSpace(s.setting(ctx, "quark_web_base")))
		if err := c.Ping(ctx); err != nil {
			out["ok"] = false
			out["web_error"] = err.Error()
		} else {
			out["web_ok"] = true
			used, total, qerr := c.Quota(ctx)
			if qerr != nil || total == 0 {
				out["quota_error"] = fmt.Sprintf("读不到容量：%v", qerr)
				if qerr == nil {
					out["quota_error"] = "夸克这次没返回容量字段"
				}
			} else {
				out["quota_ok"] = true
				out["quota_used"] = used
				out["quota_total"] = total
			}
		}
	}
	// 转存目录也一起报：用户常把「fid-目录名」整串贴进来，Ping 全绿但转存必失败。
	rawFid := strings.TrimSpace(s.setting(ctx, "quark_save_fid"))
	if rawFid != "" {
		out["save_fid"] = rawFid
		if fid := normalizeQuarkFID(rawFid); fid != rawFid {
			out["save_fid_fixed"] = fid
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// quarkFIDRe 匹配夸克的 32 位十六进制目录 fid。
var quarkFIDRe = regexp.MustCompile(`[0-9a-fA-F]{32}`)

// normalizeQuarkFID 把「40fdec…-转存」这类从界面复制来的整串清洗成纯 fid。
//
// 用户看到目录行是「转存 · 40fdec…」，很容易连名字一起贴进设置项；
// 夸克收到带后缀的 fid 会直接报参数错误，而测试接口只 Ping 不会发现。
// 这里在每次读取设置时清洗一次，历史脏数据不用手工改库。
func normalizeQuarkFID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "0" {
		return "0"
	}
	if m := quarkFIDRe.FindString(v); m != "" {
		return m
	}
	return strings.Trim(v, "/") // 认不出的原样交给夸克报错，别静默改成根目录
}

// ==================== 转存 ====================

type quarkSaveOut struct {
	Title     string
	Files     int
	FID       string // 保存目录 id
	Dir       string
	AccountID int64
	Account   string
	// Synced 是有没有顺手踢了一轮同步（库的目录正好是保存目录时）。
	Synced bool
}

// saveQuarkShareTo 把一条夸克分享转存进夸克网盘。
//
// 和 115 的 saveShareTo 一样被两处复用：网页「存到夸克」按钮与 Telegram 机器人。
func (s *Server) saveQuarkShareTo(ctx context.Context, acctID int64, raw, passcode, fid string) (*quarkSaveOut, error) {
	acctID, cr, err := s.pickQuarkAccount(ctx, acctID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cr.WebCookie) == "" {
		return nil, errNoQuarkCookie
	}
	if strings.TrimSpace(fid) == "" {
		fid = strings.TrimSpace(s.setting(ctx, "quark_save_fid"))
	}
	fid = normalizeQuarkFID(fid)

	c := quark.NewWithBase(cr.WebCookie, strings.TrimSpace(s.setting(ctx, "quark_web_base")))
	res, err := c.SaveShare(ctx, raw, passcode, fid)
	if err != nil {
		if errors.Is(err, quark.ErrCookie) {
			// 用 %w 保住错误链：上层靠 errors.Is(err, quark.ErrCookie) 决定回 401，
			// 换成普通 errors.New 就只剩 502，前端也就没法给出「去重贴 Cookie」的入口。
			return nil, fmt.Errorf("%w，请到「账号 → 夸克」重新粘贴 Cookie", err)
		}
		return nil, err
	}

	out := &quarkSaveOut{
		Title: res.Title, Files: res.Files, FID: fid,
		AccountID: acctID,
		Account:   s.accountName(ctx, acctID),
	}
	out.Dir = s.quarkDirLabel(ctx, fid)
	// 转存只是「文件进了网盘」，要重跑一次同步才会生成 strm、出现在 Emby 里。
	// 只有库的目录正好是这次保存的目录时才踢，免得每次都全量重扫。
	if libs, err := s.st.ListLibraries(ctx); err == nil {
		for _, lib := range libs {
			if libHasSource(lib, acctID, fid) {
				if job, err := s.beginSync(lib); err == nil {
					out.Synced = true
					s.watchSyncAndNotify(job, lib)
				}
				break
			}
		}
	}
	return out, nil
}

// quarkDirLabel 把 fid 翻成人看得懂的位置。夸克 TV 接口不支持按 id 查条目，
// 所以只能翻媒体库；翻不到就报 id，不假装知道目录名。
func (s *Server) quarkDirLabel(ctx context.Context, fid string) string {
	if fid == "" || fid == "0" {
		return "夸克网盘根目录"
	}
	if libs, err := s.st.ListLibraries(ctx); err == nil {
		for _, l := range libs {
			if libHasDir(l, fid) {
				return "媒体库「" + l.Name + "」"
			}
		}
	}
	return "夸克目录 " + fid
}

// handleQuarkShare POST /api/quark/share {account_id,url,share_code,passcode,fid}
func (s *Server) handleQuarkShare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID int64  `json:"account_id"`
		URL       string `json:"url"`
		ShareCode string `json:"share_code"`
		Passcode  string `json:"passcode"`
		FID       string `json:"fid"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	raw := strings.TrimSpace(body.URL)
	if raw == "" && strings.TrimSpace(body.ShareCode) != "" {
		raw = "https://pan.quark.cn/s/" + strings.TrimSpace(body.ShareCode)
	}
	if raw == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少夸克分享链接"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()

	out, err := s.saveQuarkShareTo(ctx, body.AccountID, raw, body.Passcode, body.FID)
	if err != nil {
		code := http.StatusBadGateway
		switch {
		case errors.Is(err, quark.ErrShareInvalid),
			errors.Is(err, errNoQuarkAccount), errors.Is(err, errNoQuarkCookie):
			code = http.StatusBadRequest
		case errors.Is(err, quark.ErrCookie):
			code = http.StatusUnauthorized
		}
		s.log.Warn("夸克分享转存失败", "url", raw, "err", err)
		writeErr(w, code, err)
		return
	}
	s.log.Info("夸克分享已转存", "account", out.AccountID, "title", out.Title,
		"files", out.Files, "fid", out.FID, "synced", out.Synced)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "title": out.Title, "files": out.Files,
		"fid": out.FID, "dir": out.Dir, "account_id": out.AccountID,
		"synced": out.Synced,
	})
}

// ==================== 账号与凭证 ====================

// pickQuarkAccount 找要用的夸克账号：指定了就用指定的，没指定取第一个。
func (s *Server) pickQuarkAccount(ctx context.Context, acctID int64) (int64, driverquarktv.Credential, error) {
	if acctID > 0 {
		cr, err := s.quarkCredential(ctx, acctID)
		return acctID, cr, err
	}
	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		return 0, driverquarktv.Credential{}, err
	}
	for _, a := range accounts {
		if strings.EqualFold(a.Kind, quarkKind) {
			cr, err := s.quarkCredential(ctx, a.ID)
			return a.ID, cr, err
		}
	}
	return 0, driverquarktv.Credential{}, errNoQuarkAccount
}

// quarkCredential 读出账号凭证并解析成夸克的结构。
func (s *Server) quarkCredential(ctx context.Context, acctID int64) (driverquarktv.Credential, error) {
	acct, err := s.st.GetAccount(ctx, acctID)
	if err != nil {
		return driverquarktv.Credential{}, err
	}
	if !strings.EqualFold(acct.Kind, quarkKind) {
		return driverquarktv.Credential{}, errors.New("账号 " + strconv.FormatInt(acctID, 10) + " 不是夸克TV 账号")
	}
	var cr driverquarktv.Credential
	if strings.TrimSpace(acct.Credential) != "" {
		if err := json.Unmarshal([]byte(acct.Credential), &cr); err != nil {
			return driverquarktv.Credential{}, errors.New("夸克凭证已损坏，请重新扫码登录")
		}
	}
	return cr, nil
}

// applyQuarkUpstream 把设置里的夸克接口根同步给 driver 包。
// 和 applyTMDBUpstream 一样：进程级配置，启动时与改设置时各调一次。
func (s *Server) applyQuarkUpstream(ctx context.Context) {
	driverquarktv.SetUpstream(s.setting(ctx, "quark_tv_base"), "")
}
