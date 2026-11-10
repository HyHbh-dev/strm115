//go:build !windows

package httpapi

import "syscall"

// diskUsage 返回媒体目录所在分区的总容量 / 可用容量（字节）。
// 容器里 /media 是 bind mount，Statfs 拿到的就是宿主机那块盘。
func diskUsage(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize)
}
