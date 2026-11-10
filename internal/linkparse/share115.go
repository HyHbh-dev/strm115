package linkparse

// 115 分享识别。
//
// TG 资源频道（以及绝大多数网盘资源帖）的正文都是同一套模板：
//
//	名称：沙丘2 2024 2160p
//	描述：……
//	链接：https://115.com/s/swz8k9x3?password=abcd
//	（也有写成 115.com/s/swz8k9x3 换行 提取码：abcd 的）
//
// 以前用户得点开 t.me 的帖子、复制链接、再粘到 115 —— 这里把「能交给 115 转存
// 的那一坨」直接从文本里抠出来：三个常用域名（115.com / 115cdn.com / anxia.com）、
// 密码写在 ?password=、# 后面、或者单独一行「提取码」，以及只发分享码不发链接的。
//
// 只做识别，不发任何请求；真正转存在 internal/pan 里。

import (
	"regexp"
	"strings"
)

// Share115 是一条 115 分享。
//
// URL 是按 host + code + password 拼出来的规范形式（不是原文），这样前端复制出去
// 的链接一定能打开；原文长什么样这里不留 —— 用不上，还容易把密码弄丢。
type Share115 struct {
	ShareCode string `json:"share_code"`
	Password  string `json:"password,omitempty"`
	URL       string `json:"url,omitempty"`
	Host      string `json:"host,omitempty"`
	// Matched 记的是从哪种写法认出来的：url / code
	Matched string `json:"matched,omitempty"`
}

// 115 分享的域名。115cdn.com 与 anxia.com 是同一个网盘的不同入口，
// 资源帖里三种都常见，转存时按识别到的域名拼回去即可。
const defaultShare115Host = "115.com"

var (
	// 分享链接：路径固定 /s/<code>，后面可能跟 ?password=xxx 或 #abcd。
	// 注意 code 的字符集是 [0-9a-z]，长度 6 位起 —— 太短会把 /s/ 这种路径误判成分享。
	re115ShareURL = regexp.MustCompile(
		`(?i)(?:https?://)?(?:www\.)?(115\.com|115cdn\.com|anxia\.com|115\.com\.cn)/s/([0-9a-z]{6,32})((?:[?#][^\s"'<>]*)?)`,
	)

	// URL 里的密码：?password=abcd / &pwd=abcd
	re115URLPwd = regexp.MustCompile(`(?i)[?&#](?:password|pwd|receive_code)=([0-9a-z]{2,12})`)

	// 提取码：最常见的是单独一行「提取码：abcd」。
	// 只用「提取码 / 访问码 / 口令」这类明确指向分享的标签，
	// 不用裸的「密码」——「解压密码：1234」在资源帖里太常见了，认错了会直接转存失败。
	re115PwdLabel = regexp.MustCompile(
		`(?i)(?:提取码|訪問碼|访问码|访问密码|提取密码|口令|share[_ ]?password)\s*[:：=]?\s*([0-9a-z]{4,10})`,
	)

	// 弱标签：「密码：abcd」。命中时才顺手用，且要排除「解压密码」这种。
	re115WeakPwdLabel = regexp.MustCompile(`(?i)(?:密码|密碼|pwd|password)\s*[:：=]?\s*([0-9a-z]{4,10})`)

	// 只发分享码不发链接：「分享码：swz8k9x3」
	re115CodeLabel = regexp.MustCompile(
		`(?i)(?:分享码|分享代码|share[_ ]?code)\s*[:：=]?\s*([0-9a-z]{6,32})`,
	)

	// 弱标签的排除项：同一行里出现这些词，说明这个「密码」是压缩包的不是分享的。
	re115ArchiveHint = regexp.MustCompile(`(?i)解压|压缩|rar|zip|7z|解压码`)
)

// Parse115Share 返回文本里第一条 115 分享；没有则返回 nil。
func Parse115Share(text string) *Share115 {
	all := Parse115Shares(text)
	if len(all) == 0 {
		return nil
	}
	return &all[0]
}

// Parse115Shares 返回文本里所有去重后的 115 分享（按出现顺序）。
//
// 一条帖子挂多个分享（4K / 1080p 各一条）是常事，这里全留着；
// 调用方只想要一条时用 Parse115Share。
func Parse115Shares(text string) []Share115 {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []Share115
	seen := map[string]bool{}
	add := func(s Share115) {
		key := strings.ToLower(s.ShareCode)
		if s.ShareCode == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, s)
	}

	for _, m := range re115ShareURL.FindAllStringSubmatch(text, -1) {
		host := strings.ToLower(m[1])
		code := strings.ToLower(m[2])
		pwd := ""
		if p := re115URLPwd.FindStringSubmatch(m[3]); p != nil {
			pwd = strings.ToLower(p[1])
		}
		add(Share115{ShareCode: code, Password: pwd, Host: host, Matched: "url"})
	}

	if len(out) == 0 {
		if m := re115CodeLabel.FindStringSubmatch(text); m != nil {
			add(Share115{ShareCode: strings.ToLower(m[1]), Host: defaultShare115Host, Matched: "code"})
		}
	}

	// 链接里没带密码时，用正文里的提取码补上 —— 115 分享页的密码从来不是链接的一部分，
	// 帖子作者十有八九写在下一行。
	if pwd := share115TextPassword(text); pwd != "" {
		for i := range out {
			if out[i].Password == "" {
				out[i].Password = pwd
			}
		}
	}
	for i := range out {
		out[i].URL = share115URL(out[i].Host, out[i].ShareCode, out[i].Password)
	}
	return out
}

// share115TextPassword 从正文里找提取码。先找强标签，没有再退回弱标签（避开解压密码）。
//
// 找之前先把链接本身挖掉：`?password=aaaa` 也在正文里，不挖掉的话第一条链接的密码
// 会被当成「全帖通用的提取码」，把后面那条没写密码的分享也污染了。
func share115TextPassword(text string) string {
	clean := re115ShareURL.ReplaceAllString(text, "\n")
	if m := re115PwdLabel.FindStringSubmatch(clean); m != nil {
		return strings.ToLower(m[1])
	}
	// 逐行看弱标签：只要这一行没有「解压」相关字样，就当分享密码用。
	for _, line := range strings.Split(clean, "\n") {
		if re115ArchiveHint.MatchString(line) {
			continue
		}
		if m := re115WeakPwdLabel.FindStringSubmatch(line); m != nil {
			return strings.ToLower(m[1])
		}
	}
	return ""
}

// share115URL 拼一条能直接打开的分享链接。
func share115URL(host, code, pwd string) string {
	if host == "" {
		host = defaultShare115Host
	}
	u := "https://" + host + "/s/" + code
	if pwd != "" {
		u += "?password=" + pwd
	}
	return u
}

// SetPassword 补上提取码，并同步重拼 URL。
// 用在「链接在 href 里、提取码在正文里」这种拆开写的帖子上。
func (s *Share115) SetPassword(pwd string) {
	pwd = strings.ToLower(strings.TrimSpace(pwd))
	if pwd == "" {
		return
	}
	s.Password = pwd
	s.URL = share115URL(s.Host, s.ShareCode, pwd)
}
