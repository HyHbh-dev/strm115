package scrape

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveRoot 把库里记的本地路径规整成磁盘上真正可用的绝对路径。
//
// 这个函数存在的原因是一个真踩过的坑：媒体库是在 Docker 里建的，
// local_path 记的是容器挂载点 `/media/movies`。把同一份数据库拿到 Windows
// 上直接跑时，`/media/movies` 在 Windows 上**不是**绝对路径（没有卷名），
// filepath.Join 之后会落到「当前盘符的根」，也就是 D:\media\movies ——
// 于是同步写的 .strm 在 D:\code\strm115\media\movies，
// 而刮削写的 poster.jpg 跑到了 D:\media\movies，
// Emby 读的是同步那一侧，海报永远不出现，日志里却一句错都没有。
//
// 规则：
//  1. 本身就是绝对路径且目录存在 → 原样用（Docker / Linux 下永远是这条）。
//  2. 不是绝对路径 → 退回「媒体根 + 末级目录名」，这正是容器里相对宿主机的映射关系。
//  3. 媒体根也为空 → 原样返回，让后面的写盘失败暴露出来，别静默写歪。
func ResolveRoot(localPath, mediaRoot string) string {
	p := strings.TrimSpace(localPath)
	if p == "" {
		return strings.TrimSpace(mediaRoot)
	}
	if filepath.IsAbs(p) {
		if isDir(p) {
			return p
		}
		// 绝对路径但目录不在：多半是从别的机器搬过来的库。
		// 这种情况下只做一件事——把末级目录名挂到当前媒体根下。
		if root := strings.TrimSpace(mediaRoot); root != "" {
			alt := filepath.Join(root, filepath.Base(p))
			return alt
		}
		return p
	}
	root := strings.TrimSpace(mediaRoot)
	if root == "" {
		return p
	}
	return filepath.Join(root, filepath.Base(filepath.ToSlash(p)))
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
