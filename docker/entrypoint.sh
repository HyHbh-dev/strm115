#!/bin/sh
# 容器入口：解决 bind mount 的属主问题。
#
# 为什么需要它：.strm 是写到宿主目录里的，容器内进程的 uid 必须和宿主目录属主一致，
# 否则同步会「Permission denied」。NAS 上这个 uid 通常不是 10001。
# 以 root 启动 → 按 PUID/PGID 对齐目录属主 → 立刻降权 exec，进程不留 root。
#
# 如果 compose 里写了 `user: "1026:100"`，则跳过全部逻辑，直接以该身份运行。
set -e

# 二进制路径可覆盖，便于在镜像外做脚本自测
APP_BIN="${APP_BIN:-/app/strm115}"

if [ "$(id -u)" != "0" ]; then
    exec "$APP_BIN" "$@"
fi

PUID="${PUID:-10001}"
PGID="${PGID:-10001}"

# 只在需要时重建运行用户（默认 10001:10001 就是镜像里建好的 strm）
if [ "$PUID" != "10001" ] || [ "$PGID" != "10001" ]; then
    addgroup -S -g "$PGID" strm 2>/dev/null || true
    deluser strm 2>/dev/null || true
    adduser -S -u "$PUID" -G strm -h /app strm 2>/dev/null || true
fi

# /data 很小，递归对齐没问题（目录不存在就先建）
mkdir -p "$DATA_DIR" 2>/dev/null || true
if [ -d "$DATA_DIR" ]; then
    chown -R "$PUID:$PGID" "$DATA_DIR" 2>/dev/null || true
fi

# /media 可能有几十万文件，绝不递归 chown（慢且危险）。
# 只保证顶层目录归运行用户——新建目录/文件才有正确属主。
# 已有子目录的属主不对时，日志里会明确报错，按提示在宿主上 chown 一次即可。
#
# 目录不存在基本只有一个原因：忘了挂载媒体目录。
# 这种静默失败最坑——同步会"成功"，但 .strm 写进容器层，容器一重建全没了。
if [ ! -d "$MEDIA_ROOT" ]; then
    echo "[strm115] 警告：$MEDIA_ROOT 不存在，多半是启动时没有挂载媒体目录。" >&2
    echo "          现在写入的 .strm 会留在容器可写层，容器重建即全部丢失。" >&2
    mkdir -p "$MEDIA_ROOT" 2>/dev/null || true
fi
chown "$PUID:$PGID" "$MEDIA_ROOT" 2>/dev/null || true

exec su-exec "$PUID:$PGID" "$APP_BIN" "$@"
