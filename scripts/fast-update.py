#!/usr/bin/env python3
"""快速更新：只换二进制，不重建 Docker 镜像。

为什么可行：前端是 go:embed 编进同一个二进制的，SQLite 又是纯 Go 驱动
（modernc.org/sqlite，CGO_ENABLED=0），所以本机交叉编译出的单个文件里
已经包含「后端 + 前端」，替换它等于整套程序更新，镜像一行都不用改。

更新过程：本地交叉编译 → SFTP 上传 → docker cp 进容器 → rename 覆盖 →
restart → 校验 /api/session 的 version。全程不跑 npm、不拉镜像层、不构建，
通常十几秒；ARM64 开发板原生构建要 1~3 分钟。

适用：改了 Go 代码或前端页面。
不适用：改了 Dockerfile / docker/entrypoint.sh / docker-compose.yml /
        go.mod 依赖 / .env —— 这些必须 `docker compose up -d --build`。

注意：docker cp 只改容器可写层。所以脚本最后会 `docker commit` 把这一层
固化成 `strm115:<VERSION>` 镜像 —— 之后容器被重建仍是新版。
加 `--no-commit` 可跳过（那样重建就会回退，需要重跑本脚本）。

用法（在仓库任意目录执行）：
    export SSH_HOST=192.168.1.10 SSH_USER=pi SSH_PASS='<ssh 密码>'
    python scripts/fast-update.py                      # arm64
    python scripts/fast-update.py --arch amd64         # 换架构
    python scripts/fast-update.py --host 10.0.0.5 --user pi --remote-dir /opt/strm115
    python scripts/fast-update.py --dry-run            # 只编译，不上传

目标机器全靠环境变量 / 命令行参数指定，脚本里不写任何真实地址：
    SSH_HOST / SSH_USER / SSH_PASS / SSH_REMOTE_DIR（默认 /home/<user>/strm115）
密码也可以 --password 传，但不建议（会进命令行历史）。
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def find_go() -> str:
    p = os.environ.get("GO_BIN") or shutil.which("go")
    if p:
        return p
    # Windows 上 Go 常常装在用户目录的 SDK 里，没进 PATH
    for c in (
        Path.home() / "go-sdk" / "go" / "bin" / "go.exe",
        Path("C:/go/bin/go.exe"),
    ):
        if c.exists():
            return str(c)
    sys.exit("找不到 go：请设置 GO_BIN 或把 Go 加入 PATH")


def read_version() -> str:
    v = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    if not v:
        sys.exit("VERSION 文件是空的")
    return v


def build(arch: str, version: str) -> Path:
    # go:embed all:webdist 要求构建时前端产物已经在磁盘上。
    # 新 clone 出来的仓库里只有 .gitkeep，直接编译会得到一个没有界面的二进制。
    index = ROOT / "internal" / "httpapi" / "webdist" / "index.html"
    if not index.exists():
        sys.exit(
            "前端产物不存在：先执行 `cd web && npx vite build`（产物在 "
            "internal/httpapi/webdist/，会被 go:embed 编进二进制）"
        )
    out = ROOT / ".local-tmp" / f"strm115-{arch}"
    out.parent.mkdir(exist_ok=True)
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch)
    cmd = [
        find_go(), "build", "-trimpath",
        "-ldflags", f"-s -w -X main.Version={version}",
        "-o", str(out), "./cmd/server",
    ]
    print(f"[1/4] 交叉编译 linux/{arch}（version={version}）…", flush=True)
    subprocess.run(cmd, cwd=str(ROOT), env=env, check=True)
    size = out.stat().st_size / 1024 / 1024
    print(f"      产物 {out.relative_to(ROOT)}  {size:.1f} MB", flush=True)
    return out


def ssh_exec(cli, cmd: str, timeout: int = 300) -> tuple[str, str, int]:
    _, stdout, stderr = cli.exec_command(cmd, timeout=timeout)
    out = stdout.read().decode("utf-8", "replace")
    err = stderr.read().decode("utf-8", "replace")
    code = stdout.channel.recv_exit_status()
    return out, err, code


def main() -> int:
    ap = argparse.ArgumentParser(description="只换二进制，不重建镜像")
    ap.add_argument("--host", default=os.environ.get("SSH_HOST", ""))
    ap.add_argument("--user", default=os.environ.get("SSH_USER", ""))
    ap.add_argument("--password", default=os.environ.get("SSH_PASS", ""))
    ap.add_argument("--remote-dir", default=os.environ.get("SSH_REMOTE_DIR", ""))
    ap.add_argument("--container", default="strm115")
    ap.add_argument("--arch", default="arm64", choices=["arm64", "amd64"])
    ap.add_argument("--image-tag", default="", help="固化镜像的 tag，默认 strm115:<VERSION>")
    ap.add_argument("--no-commit", action="store_true", help="跳过固化镜像（容器重建会回到旧二进制）")
    ap.add_argument("--dry-run", action="store_true", help="只编译，不上传")
    a = ap.parse_args()

    version = read_version()
    binary = build(a.arch, version)
    if a.dry_run:
        print("--dry-run：到此为止，没有动远程。")
        return 0

    if not a.host:
        ap.error("请设置 SSH_HOST（或 --host）：脚本不内置任何部署目标")
    if not a.user:
        ap.error("请设置 SSH_USER（或 --user）")
    if not a.remote_dir:
        a.remote_dir = f"/home/{a.user}/strm115"

    if not a.password:
        sys.exit("请设置环境变量 SSH_PASS（或用 --password）")
    try:
        import paramiko
    except ImportError:
        sys.exit("缺 paramiko：pip install paramiko")

    remote_tmp = f"{a.remote_dir}/.strm115-{a.arch}.new"
    backup = f"{a.remote_dir}/strm115.backup"

    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    cli.connect(a.host, username=a.user, password=a.password, timeout=20,
                look_for_keys=False, allow_agent=False)
    try:
        print(f"[2/4] 上传二进制到 {a.host}:{remote_tmp} …", flush=True)
        sftp = cli.open_sftp()
        sftp.put(str(binary), remote_tmp)
        sftp.chmod(remote_tmp, 0o755)
        sftp.close()

        print("[3/4] 替换容器内二进制并重启 …", flush=True)
        steps = [
            # 先把镜像里那份旧二进制备份到宿主机，验证失败时可以一键回滚
            f"docker cp {a.container}:/app/strm115 {backup}",
            f"docker cp {remote_tmp} {a.container}:/app/strm115.new",
            f"docker exec {a.container} sh -c 'chmod 755 /app/strm115.new && mv -f /app/strm115.new /app/strm115'",
            f"docker restart {a.container}",
            f"rm -f {remote_tmp}",
        ]
        for step in steps:
            out, err, code = ssh_exec(cli, f"cd {a.remote_dir} && {step}")
            if code != 0:
                print(f"      失败：{step}\n{out}{err}", file=sys.stderr)
                return 1
            if out.strip():
                print("     ", out.strip().splitlines()[-1])

        print("[4/4] 校验 /api/session …", flush=True)
        check = (f"docker exec {a.container} wget -q -O - -T 3 "
                 f"http://127.0.0.1:6060/api/session")
        ok = False
        for _ in range(20):
            out, _, code = ssh_exec(cli, check, timeout=20)
            if code == 0:
                try:
                    got = json.loads(out.strip()).get("version", "")
                except Exception:
                    got = ""
                if got == version:
                    print(f"      ✓ 运行版本 {got}，更新完成")
                    ok = True
                    break
            time.sleep(2)
        if not ok:
            print("      ✗ 版本校验没过，正在回滚到上一版二进制…", file=sys.stderr)
            rollback = [
                f"docker cp {backup} {a.container}:/app/strm115.new",
                f"docker exec {a.container} sh -c 'chmod 755 /app/strm115.new && mv -f /app/strm115.new /app/strm115'",
                f"docker restart {a.container}",
            ]
            for step in rollback:
                ssh_exec(cli, f"cd {a.remote_dir} && {step}")
            return 1

        # docker cp 只改了容器可写层：容器一旦被 compose 重建就会退回镜像里的旧版。
        # 用 docker commit 把这一层固化成 <tag> 镜像，之后即便重建也还是新二进制。
        # 这比重新 build 便宜得多（只提交一个 15MB 的层），也不用拉任何依赖。
        if not a.no_commit:
            tag = a.image_tag or f"strm115:{version}"
            out, err, code = ssh_exec(cli, f"docker commit {a.container} {tag}", timeout=180)
            if code == 0:
                print(f"      ✓ 已固化镜像 {tag}，容器重建后仍是 {version}")
            else:
                print(f"      ! 固化镜像失败（不影响当前运行）：{err.strip()}", file=sys.stderr)
        return 0
    finally:
        cli.close()


if __name__ == "__main__":
    sys.exit(main())
