package main

// backup 子命令：同一个二进制顺手提供在线备份，容器里不用再装 sqlite3 CLI。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"strm115/internal/config"
	"strm115/internal/store"
)

func runBackup(args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "用法: strm115 backup <输出文件绝对路径>")
		return 2
	}
	cfg := config.Load()
	src := filepath.Join(cfg.DataDir, "strm115.db")
	if _, err := os.Stat(src); err != nil {
		fmt.Fprintf(os.Stderr, "找不到数据库 %s: %v\n", src, err)
		return 1
	}
	st, err := store.Open(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		return 1
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := st.BackupTo(ctx, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "备份失败: %v\n", err)
		return 1
	}
	fi, err := os.Stat(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "备份已写出但读取文件信息失败: %v\n", err)
		return 1
	}
	fmt.Printf("备份完成: %s (%d 字节)\n", args[0], fi.Size())
	return 0
}
