package httpapi

// 管理员账号密码放在数据库的 settings 表里，而不是环境变量里。
//
// 为什么：密码是「运行期配置」，改一次要重建容器是没道理的；
// 而且 .env 是明文、会进备份、会被顺手发到群里。放进 SQLite 后，
// 首次访问网页时设置，之后在「设置 → 管理员账号」里改，容器不用动。
//
// 存储的是 PBKDF2-HMAC-SHA256 派生值（含迭代次数和 salt），
// 不存明文，也不存可逆的加密值 —— 数据库被拿走也读不出密码。
// 环境变量 ADMIN_USER / ADMIN_PASSWORD 只作为「首次启动的初始值」：
// 数据库里已经有账号时它会被忽略，避免改网页密码后被环境变量悄悄覆盖回去。

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	keyAdminUser = "admin_user"
	keyAdminPass = "admin_pass"

	// 迭代次数写进哈希串里，将来调高不需要迁移旧记录：
	// 旧记录仍按自己的次数校验，用户下次改密码时自然升级。
	pbkdf2Iters = 120000
	pbkdf2Len   = 32
)

// ==================== 密码哈希 ====================

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	var out []byte
	buf := make([]byte, hashLen)
	u := make([]byte, hashLen)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		prf.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		buf = prf.Sum(buf[:0])
		copy(u, buf)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range buf {
				buf[i] ^= u[i]
			}
		}
		out = append(out, buf...)
	}
	return out[:keyLen]
}

// hashPassword 产出 "pbkdf2-sha256$<iter>$<salt>$<hash>"，全部 base64url 之外的部分保持可读。
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iters, pbkdf2Len)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		pbkdf2Iters,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// verifyPassword 用常量时间比较，避免用响应时间一位一位试出密码。
func verifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 || iter > 5_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ==================== 凭据状态 ====================

// credsReady 判断是否已经设置过管理员账号。
func (s *Server) credsReady() bool {
	s.credMu.RLock()
	defer s.credMu.RUnlock()
	return s.credUser != "" && s.credHash != ""
}

func (s *Server) currentUser() string {
	s.credMu.RLock()
	defer s.credMu.RUnlock()
	return s.credUser
}

func (s *Server) setCreds(user, hash string) {
	s.credMu.Lock()
	s.credUser, s.credHash = user, hash
	s.credMu.Unlock()
}

// loadCreds 从数据库读一次；没有就返回 false。
func (s *Server) loadCreds(ctx context.Context) (user, hash string, ok bool) {
	u, okUser := s.st.GetSetting(ctx, keyAdminUser)
	h, okPass := s.st.GetSetting(ctx, keyAdminPass)
	if !okUser || !okPass || u == "" || h == "" {
		return "", "", false
	}
	return u, h, true
}

// initCreds 启动时调用：数据库优先，其次是环境变量（便于无人值守部署）。
func (s *Server) initCreds(ctx context.Context) {
	if user, hash, ok := s.loadCreds(ctx); ok {
		s.setCreds(user, hash)
		return
	}
	if s.cfg.AdminPass == "" {
		// 默认形态：首次打开网页时设置。这里给一句提示，方便看 docker logs 的人知道下一步。
		s.log.Info(fmt.Sprintf("管理员账号未初始化：打开 http://<本机IP>%s 设置账号密码（也可用 ADMIN_USER/ADMIN_PASSWORD 预置）", s.cfg.Addr))
		return
	}
	user := strings.TrimSpace(s.cfg.AdminUser)
	if user == "" {
		user = "admin"
	}
	hash, err := hashPassword(s.cfg.AdminPass)
	if err != nil {
		s.log.Error("初始管理员密码处理失败", "err", err)
		return
	}
	if err := s.saveCreds(ctx, user, hash); err != nil {
		s.log.Error("写入初始管理员账号失败", "err", err)
		return
	}
	s.log.Info(fmt.Sprintf("已用环境变量初始化管理员账号 %q（之后可在网页里修改）", user))
}

func (s *Server) saveCreds(ctx context.Context, user, hash string) error {
	if err := s.st.SetSetting(ctx, keyAdminUser, user); err != nil {
		return err
	}
	if err := s.st.SetSetting(ctx, keyAdminPass, hash); err != nil {
		return err
	}
	s.setCreds(user, hash)
	return nil
}

// ==================== 会话 ====================

func (s *Server) setSessionCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 只有前置 HTTPS 反代时才开（COOKIE_SECURE=1）。明文 HTTP 下带 Secure
		// 浏览器不会回传，登录会立刻掉线，所以默认关。
		Secure: s.cfg.CookieSecure,
		MaxAge: int(sessionTTL.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: "sid", Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.cfg.CookieSecure, MaxAge: -1,
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"setup_needed": !s.credsReady(),
		"authed":       s.credsReady() && s.sess.valid(r),
		"user":         s.currentUser(),
		"version":      Version,
		"uptime_sec":   int(time.Since(s.start).Seconds()),
	})
}

// ==================== 首次设置 ====================

type setupReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func validateCredInput(user, pass string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return "", errors.New("账号不能为空")
	}
	if len([]rune(user)) > 32 {
		return "", errors.New("账号太长（最多 32 个字符）")
	}
	if strings.ContainsAny(user, " \t\r\n") {
		return "", errors.New("账号里不能有空格")
	}
	if len(pass) < 6 {
		return "", errors.New("密码至少 6 位")
	}
	if len(pass) > 200 {
		return "", errors.New("密码太长")
	}
	return user, nil
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.credsReady() {
		// 已经初始化过就不能再走这条路，否则任何人打开网页都能改密码
		writeErr(w, http.StatusConflict, errors.New("已经设置过管理员账号"))
		return
	}
	var req setupReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	user, err := validateCredInput(req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("密码处理失败"))
		return
	}
	if err := s.saveCreds(r.Context(), user, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info(fmt.Sprintf("管理员账号已初始化：%s", user))

	tok := s.sess.create()
	s.setSessionCookie(w, tok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": user})
}

// ==================== 登录 / 登出 ====================

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.credsReady() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "尚未设置管理员账号密码，请刷新页面完成设置", "setup_needed": "1"})
		return
	}
	// 防爆破：同一来源连续失败 5 次锁 5 分钟。放在读请求体之前，
	// 被锁时连 PBKDF2 都不用算。
	guardKey := clientKey(r)
	if s.login != nil {
		if wait := s.login.blocked(guardKey, time.Now()); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests,
				fmt.Errorf("登录失败次数过多，请 %d 秒后再试", int(wait.Seconds())+1))
			return
		}
	}
	var req loginReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}

	s.credMu.RLock()
	wantUser, wantHash := s.credUser, s.credHash
	s.credMu.RUnlock()

	userOK := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Username)), []byte(wantUser)) == 1
	passOK := verifyPassword(wantHash, req.Password)
	if !userOK || !passOK {
		// 统一延迟：不让响应时间透露「账号对不对」
		time.Sleep(400 * time.Millisecond)
		if s.login != nil {
			s.login.fail(guardKey, time.Now())
		}
		writeErr(w, http.StatusUnauthorized, errors.New("账号或密码错误"))
		return
	}
	if s.login != nil {
		s.login.clear(guardKey)
	}
	tok := s.sess.create()
	s.setSessionCookie(w, tok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": wantUser})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sess.drop(r)
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ==================== 修改管理员账号 / 密码 ====================

type credChangeReq struct {
	CurrentPassword string `json:"current_password"`
	Username        string `json:"username"`
	Password        string `json:"password"`
}

func (s *Server) handleChangeCreds(w http.ResponseWriter, r *http.Request) {
	var req credChangeReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	s.credMu.RLock()
	wantHash := s.credHash
	s.credMu.RUnlock()

	if !verifyPassword(wantHash, req.CurrentPassword) {
		time.Sleep(400 * time.Millisecond)
		writeErr(w, http.StatusForbidden, errors.New("当前密码不正确"))
		return
	}
	user, err := validateCredInput(req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("密码处理失败"))
		return
	}
	if err := s.saveCreds(r.Context(), user, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// 换密码等于踢掉所有旧会话，然后给当前浏览器发一张新的，用户不会被自己踢出去。
	s.sess.reset()
	tok := s.sess.create()
	s.setSessionCookie(w, tok)
	s.log.Info(fmt.Sprintf("管理员账号已更新：%s", user))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": user})
}
