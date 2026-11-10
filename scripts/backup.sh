#!/bin/sh
# strm115 在线数据库备份（在宿主机、项目根目录执行）。
#
# 为什么必须走 `strm115 backup`：SQLite 开 WAL 时，直接 cp strm115.db 会漏掉
# -wal 里尚未 checkpoint 的事务，可能拿到不一致甚至损坏的快照。backup 子命令
# 内部用 VACUUM INTO 做在线一致性快照，不需要停服，也不影响在途请求。
# /data 必须是本地盘：SQLite 依赖文件锁语义，放 NFS/SMB 上可能损坏数据库，
# 备份文件同样不要写到网络盘。
#
# 用法：
#   ./scripts/backup.sh                # 输出到 ./backups，保留最近 14 份
#   ./scripts/backup.sh /srv/backup    # 指定输出目录
#   KEEP=30 ./scripts/backup.sh        # 只保留最近 30 份
# 若生产是用叠加文件启动的，先设置（docker compose 会自动读取）：
#   COMPOSE_FILE=docker-compose.yml:docker-compose.prod.yml
#   COMPOSE_ENV_FILES=.env.production

set -eu

# Windows 的 Git Bash 会把 /app/strm115 这类参数误转成本地盘符路径
# （exec 报 "C:/Program Files/.../app/strm115: no such file"）。
# 关掉 MSYS 路径转换；Linux/NAS 原生 sh 下这只是个无害的空环境变量。
MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

OUT_DIR="${1:-./backups}"
KEEP="${KEEP:-14}"

STAMP="$(date +%Y%m%d-%H%M%S)"
TMP_NAME="backup-${STAMP}.db"
CONTAINER_PATH="/data/${TMP_NAME}"

mkdir -p "$OUT_DIR"

# 无论成功还是中途失败，都删掉容器内临时快照，避免 /data 里堆积。
cleanup() {
    docker compose exec -T strm115 rm -f "$CONTAINER_PATH" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

echo "==> 在容器内生成一致性快照：${CONTAINER_PATH}"
docker compose exec -T strm115 /app/strm115 backup "$CONTAINER_PATH"

echo "==> 拷贝快照到宿主机：${OUT_DIR}/${TMP_NAME}"
docker compose cp "strm115:${CONTAINER_PATH}" "${OUT_DIR}/${TMP_NAME}"

echo "==> 清理容器内临时文件"
cleanup
trap - EXIT HUP INT TERM

echo "==> 只保留最近 ${KEEP} 份备份"
# 只匹配本脚本产出的 backup-*.db，避免误删输出目录里的其它文件。
( cd "$OUT_DIR" && ls -1t backup-*.db 2>/dev/null | tail -n +$((KEEP + 1)) | xargs -r rm -- )

echo "==> 备份完成：${OUT_DIR}/${TMP_NAME}"
