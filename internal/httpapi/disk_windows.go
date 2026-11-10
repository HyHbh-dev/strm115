//go:build windows

package httpapi

// diskUsage 在 Windows 本地直接跑（不经过 Docker）时拿不到 /proc，
// 也不想为了一个首页数字引入 syscall 的平台分支，所以如实返回「未知」：
// 首页会把磁盘那一行显示成「—」，CPU / 内存 / 网盘容量照常。
// 正常部署走 Docker（Linux），由 disk_unix.go 提供真实数值。
func diskUsage(string) (total, free uint64) { return 0, 0 }
