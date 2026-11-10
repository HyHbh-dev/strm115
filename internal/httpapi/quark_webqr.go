package httpapi

// 网页 Cookie 的扫码登录。
//
// 之前只能开 F12 从 Network 里复制 Cookie；这里把夸克网页自己的 CAS 扫码
// 流程搬进来，手机上确认一下就把 Cookie 写进同一个账号。拿到的 Cookie 和
// 手贴的完全等价，之后由 quarkKeep 自动续期。

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"strm115/internal/quark"
)

type quarkWebSession struct {
	login     *quark.QRLogin
	accountID int64
	born      time.Time
	done      bool
}

var qkWebMu sync.Mutex
var qkWeb = map[string]*quarkWebSession{}

// handleQuarkWebQRStart POST /api/accounts/quarktv/web-qr {account_id}
//
// 返回 png 是 data:image/png;base64,…（和 115 那个 JPEG 不同，别抄错前缀）。
func (s *Server) handleQuarkWebQRStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID int64 `json:"account_id"`
	}
	_ = readJSON(r, &body)
	if body.AccountID == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("先选一个夸克账号"))
		return
	}
	acct, err := s.st.GetAccount(r.Context(), body.AccountID)
	if err != nil || !strings.EqualFold(acct.Kind, quarkKind) {
		writeErr(w, http.StatusBadRequest, errors.New("这个账号不是夸克TV 账号"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	q := quark.NewQRLogin()
	qrURL, err := q.Start(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	png, err := qrcode.Encode(qrURL, qrcode.Medium, 256)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	id := randToken()
	qkWebMu.Lock()
	// 一个账号只留一张活码：用户连点几次「刷新」时，旧会话全部作废，
	// 免得手机上残留的旧码还能撞上某个后台轮询。
	for k, old := range qkWeb {
		if old.accountID == body.AccountID {
			delete(qkWeb, k)
		}
	}
	qkWeb[id] = &quarkWebSession{login: q, accountID: body.AccountID, born: time.Now()}
	qkWebMu.Unlock()

	// 夸克服务端约 120 秒作废 token，这里按 110 秒给前端倒计时，
	// 提前换码而不是让用户扫到「登录请求过期」。
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"png":        "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		"qr_url":     qrURL,
		"expires_in": int(quark.QRTokenTTL / time.Second),
	})
}

// handleQuarkWebQRPoll GET /api/accounts/quarktv/web-qr/{id}
//
// 没确认时 waiting；上游判作废时 expired（前端会自动换一张新码）；
// 确认后当场校验 Cookie 并写回账号，再点亮保活。
func (s *Server) handleQuarkWebQRPoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	qkWebMu.Lock()
	sess := qkWeb[id]
	if sess == nil {
		qkWebMu.Unlock()
		writeErr(w, http.StatusNotFound, errors.New("二维码会话不存在或已过期"))
		return
	}
	if time.Since(sess.born) > quark.QRTokenTTL {
		delete(qkWeb, id)
		qkWebMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "expired",
			"msg":    "二维码已过期，正在换一张新的",
		})
		return
	}
	if sess.done {
		qkWebMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "cookie_set": true})
		return
	}
	qkWebMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	cookie, done, err := sess.login.Poll(ctx)
	if err != nil {
		// 上游 50004002 / "Token Not Found"：再轮询下去永远是同一句，
		// 直接告诉前端换码，别把它当成网络错误弹一堆红字。
		if errors.Is(err, quark.ErrQRExpired) {
			qkWebMu.Lock()
			delete(qkWeb, id)
			qkWebMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "expired",
				"msg":    "夸克提示登录请求已过期，正在换一张新的",
			})
			return
		}
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"status": "waiting"})
		return
	}
	if err := s.saveQuarkCookie(ctx, sess.accountID, cookie); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	qkWebMu.Lock()
	sess.done = true
	qkWebMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "cookie_set": true})
}

// cleanQuarkWebSessions 由 web.go 的 janitor 调用，清掉过期会话。
func cleanQuarkWebSessions(cut time.Time) {
	qkWebMu.Lock()
	defer qkWebMu.Unlock()
	for k, v := range qkWeb {
		if v.born.Before(cut) {
			delete(qkWeb, k)
		}
	}
}
