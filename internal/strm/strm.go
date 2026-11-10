// Package strm 负责把「网盘里的一个视频文件」变成「本地一个 .strm 文件」。
//
// 这里全部是纯函数（除了最后的写盘），因为它是整条链路里最容易被拿来
// 反复调试的部分：改名规则、扩展名、路径拼接、非法字符，每一条都能单测。
package strm

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// 默认视频扩展名。ISO 单独处理：它是光盘镜像，Emby 需要靠扩展名认出来。
var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".ts": true, ".m2ts": true, ".avi": true,
	".mov": true, ".wmv": true, ".flv": true, ".rmvb": true, ".webm": true,
	".mpg": true, ".mpeg": true, ".m4v": true, ".iso": true, ".rm": true,
	".asf": true, ".vob": true, ".3gp": true, ".divx": true, ".ogv": true,
}

// IsVideo 判断文件名是不是视频。
func IsVideo(name string) bool {
	return videoExts[strings.ToLower(path.Ext(name))]
}

// adPhrases 是发布组塞进资源目录的「广告视频」典型字样。
//
// 这类文件不是正片：有的整名就是一句广告（`【更多无水印高品质资源请访问
// www.Butailing.com】【…】.mkv`），有的是公众号/群号宣传，有的干脆叫
// `广告.mp4`。同步时它们会被当成视频写成 .strm，Emby 里就多出一张灰卡片 ——
// 「刮削多出了一个广告」说的就是这个。
var adPhrases = []string{
	"http", // 文件名里带链接的一律不是正片（http:// 与 https:// 都命中）
	"资源请访问", "更多无水印", "无水印高品质", "更多高清资源", "最新网址",
	"永久网址", "永久地址", "防失联", "获取更多资源", "免费观看",
	"关注公众号", "扫码关注", "加微信", "加qq群", "更多资源请",
}

// adOnlyNames 是「整个名字就是这个意思」的短文件名（去掉扩展名后）。
var adOnlyNames = map[string]bool{
	"广告": true, "片头广告": true, "广告视频": true, "宣传片": true,
}

var (
	// 括号里的发布组前缀/广告语：`【…】` `[…]` `（…）` `(…)`。
	bracketRe = regexp.MustCompile(`【[^】]*】|\[[^\]]*\]|（[^）]*）|\([^)]*\)`)
	// 像片名的标志：年份或分辨率/片源。广告名里不会有这些。
	titleYearRe = regexp.MustCompile(`(?:19|20)\d{2}`)
	titleResRe  = regexp.MustCompile(`(?i)(2160p|1080p|720p|480p|4k|uhd|bluray|blu-ray|remux|web-?dl|webrip)`)
)

// IsAd 判断一个视频文件是不是发布组塞的广告，别给它写 .strm。
//
// 三条判据，都收得很紧，宁可漏掉也不能误杀正片：
//
//  1. 整个名字就是一个广告词（`广告.mkv`、`片头广告.mp4`）；
//  2. 把所有括号内容剥掉后什么都不剩 —— 整名都是 `【更多无水印…】` 这种；
//  3. 命中广告短语，而且名字里既没有年份也没有分辨率/片源标记。
//
// 第 3 条的年/分辨率条件是为了放过「发布组前缀 + 正片名」这种正经命名：
// `【高清影视之家发布 www.BBEGGE.com】极速：伪装者[简繁英字幕].2026.2160p`
// 含广告短语，但它是正片。
func IsAd(name string) bool {
	base := strings.TrimSuffix(name, path.Ext(name))
	if adOnlyNames[strings.Trim(base, " \t【】[]()（）_-—·.、!！|")] {
		return true
	}
	if strings.TrimSpace(stripBrackets(base)) == "" && bracketRe.MatchString(base) {
		return true
	}
	low := strings.ToLower(base)
	for _, p := range adPhrases {
		if strings.Contains(low, p) {
			if titleYearRe.MatchString(base) || titleResRe.MatchString(base) {
				return false
			}
			return true
		}
	}
	return false
}

// stripBrackets 反复剥括号直到没有嵌套残留，再把剩下的标点去掉。
func stripBrackets(s string) string {
	for {
		out := bracketRe.ReplaceAllString(s, "")
		if out == s {
			break
		}
		s = out
	}
	return strings.Trim(s, " \t.、,，-—·|")
}

// Ext 返回小写扩展名（含点），没有则空串。
func Ext(name string) string { return strings.ToLower(path.Ext(name)) }

// PlayID 是直链里那一段标识。目前 115 就是 pickcode。
// 单独抽出来是为了将来接夸克/阿里时，各网盘的标识格式在这里统一。
func PlayID(pickCode string) string { return pickCode }

// QuarkPrefix 是夸克文件在播放地址里的前缀。
//
// 为什么必须带前缀：115 的 pickcode 和夸克的 fid 都是 32 位十六进制字符串，
// 光看地址分不出这条是哪家的文件。一个库同时挂两个网盘之后，播放代理要拿
// 「对的凭证」去换直链——拿 115 的 Cookie 去问夸克的 fid 只会得到 502。
//
// 115 不加前缀：历史 .strm 全是 /d/{pickcode}，一改就等于让整个库播不了。
const QuarkPrefix = "q/"

// PlayIDForKind 按网盘类型生成播放标识（写在 .strm 的 /d/ 后面）。
//
//	115          → bf4l2rr2j5vfa8s5l
//	quarktv      → q/40fdec135a084516ac8d66ea19dddbb9
func PlayIDForKind(kind, id string) string {
	if kind == "quarktv" {
		return QuarkPrefix + id
	}
	return id
}

// SplitPlayKind 把播放标识拆回「网盘类型 + 纯文件 id」。
//
// 返回的 kind 为空表示「老格式，不知道是哪个网盘」——这种地址只能靠台账
// 反查账号，查不到才轮到兜底账号。
func SplitPlayKind(playID string) (kind, id string) {
	if strings.HasPrefix(playID, QuarkPrefix) {
		return "quarktv", strings.TrimPrefix(playID, QuarkPrefix)
	}
	return "", playID
}

// Content 生成 .strm 的内容。
//
// 两种形态：
//
//	保留扩展名（默认）： http://nas:6086/d/{pickcode}.mkv?/电影名 (2020).mkv
//	不保留扩展名：        http://nas:6086/d/{pickcode}?/电影名 (2020).mkv
//
// 路径段里带扩展名是因为：Emby / 各类播放器在决定「能不能直放」时看的是 URL 的
// 最后一段扩展名，而不是我们的响应头；不带扩展名会被当成未知类型从而要求转码。
//
// `?/文件名` 那一段纯装饰，但很有用：它让 Emby 不用额外发 HEAD 请求就知道
// 这是什么文件。注意 `#` 必须转义——不转的话它后面会被当成 URL fragment 截掉。
func Content(domain, playID string, keepExt bool, fileName string) string {
	base := strings.TrimRight(strings.TrimSpace(domain), "/")
	idPart := playID
	if keepExt {
		idPart += Ext(fileName)
	}
	if fileName == "" {
		return fmt.Sprintf("%s/d/%s", base, idPart)
	}
	return fmt.Sprintf("%s/d/%s?/%s", base, idPart, escapeName(fileName))
}

func escapeName(n string) string {
	r := strings.NewReplacer("%", "%25", "#", "%23", "?", "%3F")
	return r.Replace(n)
}

// Target 算出 .strm 的落盘路径。
// relDir 是相对媒体库根目录的目录（用 "/" 分隔），fileName 是原视频文件名。
func Target(root, relDir, fileName string) string {
	base := strings.TrimSuffix(Sanitize(fileName), path.Ext(fileName))
	if base == "" {
		base = "unnamed"
	}
	// ISO 保留 .iso.strm：Emby 对光盘镜像的判断依赖双扩展名
	if strings.EqualFold(path.Ext(fileName), ".iso") {
		base += ".iso"
	}
	dir := filepath.Join(root, filepath.FromSlash(CleanRel(relDir)))
	return filepath.Join(dir, base+".strm")
}

// CleanRel 规范化相对路径：处理 ".."、多余的斜杠，并清掉会逃出媒体根的层级。
// 这是唯一的安全边界——网盘里的目录名是别人可以起的（分享转存进来的也会走这里）。
//
// ".." 用「弹栈」而不是「直接丢弃」：`a/b/../c` 应该得到 `a/c`；
// 但弹出到根之上的一律忽略，所以 `../../etc` 只会变成 `etc`，永远出不了媒体根。
func CleanRel(rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	parts := make([]string, 0, 8)
	for _, p := range strings.Split(rel, "/") {
		p = strings.TrimSpace(p)
		switch p {
		case "", ".":
			continue
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
			continue
		}
		parts = append(parts, Sanitize(p))
	}
	return strings.Join(parts, "/")
}

// Sanitize 把一段名字变成各平台都能落盘的形式。
// 保留中文和空格——剧集名靠它给 Emby 认。
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteRune('_')
		default:
			if unicode.IsControl(r) {
				continue
			}
			b.WriteRune(r)
		}
	}
	out := strings.TrimRight(b.String(), " .")
	if len(out) > 180 {
		// 按字节截断会切坏 UTF-8，按字符截
		rs := []rune(out)
		if len(rs) > 120 {
			out = string(rs[:120])
		}
	}
	return out
}

// Write 原子写：先写 .tmp 再 rename。
// 直接写目标文件的话，进程被杀会留下半截文件，Emby 会当成一个 0 字节的坏条目。
//
// 权限刻意用 0755/0644 而不是 0750/0640：媒体服务器多半跑在另一个容器里，
// 它的 uid 与本服务的 PUID 不一定相同。用 0640 时 Emby 连 .strm 都读不到，
// 表现为「库扫到了但一个条目都不出来」，而且报错很不直观。
// .strm 里只有一行播放 URL，没有任何凭证，没必要按私密文件对待。
func Write(filePath, content string) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return err
	}
	tmp := filePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filePath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Read 读回一个 .strm，用于判断内容是否变化（避免无谓的写盘和 Emby 重扫）。
func Read(filePath string) (string, error) {
	b, err := os.ReadFile(filePath)
	return strings.TrimSpace(string(b)), err
}
