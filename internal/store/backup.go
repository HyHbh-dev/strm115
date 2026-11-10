package store

// SQLite 在线一致性备份。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BackupTo 用 VACUUM INTO 写出一份完整快照，服务不用停。
//
// 为什么不能直接 cp strm115.db：WAL 模式下已提交的事务可能还在 -wal 里，
// 只拷主库会拿到「半截」状态（甚至打不开）。VACUUM INTO 由 SQLite 自己
// 读一致性视图写新文件，顺便把碎片整理掉，是官方推荐的在线备份方式。
func (s *Store) BackupTo(ctx context.Context, dest string) error {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return fmt.Errorf("备份路径为空")
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("备份文件已存在，拒绝覆盖: %s", dest)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查备份路径失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("创建备份目录失败: %w", err)
	}
	// VACUUM INTO 后面跟的是文件名表达式，不能用 ? 绑定；转义单引号即可。
	q := "VACUUM INTO '" + strings.ReplaceAll(filepath.ToSlash(dest), "'", "''") + "'"
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		// 失败可能留下半截文件，删掉免得后来者误以为是好备份。
		_ = os.Remove(dest)
		return fmt.Errorf("VACUUM INTO 失败: %w", err)
	}
	return nil
}
