package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupToCreatesConsistentSnapshot(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS backup_probe (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO backup_probe(v) VALUES('中文数据')`); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.BackupTo(ctx, dest); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("备份文件不存在或为空: %v, %v", fi, err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dest))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT v FROM backup_probe`).Scan(&got); err != nil {
		t.Fatalf("快照里读不到刚写入的数据: %v", err)
	}
	if got != "中文数据" {
		t.Fatalf("快照内容 = %q", got)
	}

	// 再写一条，快照应该是「当时」的状态而不是跟着变。
	if _, err := s.db.Exec(`INSERT INTO backup_probe(v) VALUES('后来的')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM backup_probe`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("快照必须是备份那一刻的一致性视图, count=%d err=%v", n, err)
	}

	if err := s.BackupTo(ctx, dest); err == nil {
		t.Fatal("目标已存在时必须拒绝覆盖")
	}
	if err := s.BackupTo(ctx, ""); err == nil {
		t.Fatal("空路径必须报错")
	}
}

func TestBackupToRejectsUnwritableDir(t *testing.T) {
	s := openTest(t)
	// 用一个「已存在的文件」当父目录，MkdirAll 必然失败。
	base := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.BackupTo(context.Background(), filepath.Join(base, "x.db")); err == nil {
		t.Fatal("父路径不是目录时应报错")
	}
}
