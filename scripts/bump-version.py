#!/usr/bin/env python3
"""统一更新项目版本号。

用法（在仓库根目录执行）：
    python scripts/bump-version.py            # 自动把 patch 加一：v0.2.0 → v0.2.1
    python scripts/bump-version.py v0.3.0    # 指定版本
    python scripts/bump-version.py --minor   # 加一个次版本：v0.2.1 → v0.3.0

为什么要有这个脚本：版本号散落在 Dockerfile / compose / main.go / package.json /
前端占位文案里，手工改总会漏一处，然后界面上显示 v0.1.0、镜像却是 v0.2.0，
排查「改的代码到底生效没有」时能白白耗掉半小时。VERSION 是唯一权威来源，
其余位置全部由这里同步改写。

约定：**每次修复或功能更新都跑一次**，提交时把 VERSION 一起带上。
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
VERSION_FILE = ROOT / "VERSION"


def read_version() -> str:
    return VERSION_FILE.read_text(encoding="utf-8").strip()


def parse(v: str) -> tuple[int, int, int]:
    m = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)", v.strip())
    if not m:
        raise SystemExit(f"版本号格式不对：{v!r}，应形如 v0.2.0")
    return int(m[1]), int(m[2]), int(m[3])


def next_version(arg: str | None) -> str:
    major, minor, patch = parse(read_version())
    if arg in (None, "--patch", ""):
        patch += 1
    elif arg == "--minor":
        minor, patch = minor + 1, 0
    elif arg == "--major":
        major, minor, patch = major + 1, 0, 0
    else:
        major, minor, patch = parse(arg)
    return f"v{major}.{minor}.{patch}"


def sub(path: str, pattern: str, repl: str, count: int = 1) -> None:
    p = ROOT / path
    if not p.exists():
        print(f"  跳过（文件不存在）: {path}")
        return
    text = p.read_text(encoding="utf-8", newline="")
    new, n = re.subn(pattern, repl, text, count=count)
    if n == 0:
        print(f"  ⚠ 没匹配到，请手工检查: {path}  <- {pattern}")
        return
    p.write_text(new, encoding="utf-8", newline="")
    print(f"  ✓ {path} ({n} 处)")


def main() -> None:
    arg = sys.argv[1] if len(sys.argv) > 1 else None
    old = read_version()
    new = next_version(arg)
    print(f"{old}  →  {new}")

    # 1) 权威来源
    VERSION_FILE.write_text(new + "\n", encoding="utf-8", newline="\n")
    print("  ✓ VERSION")

    # 2) 各处默认值（用「捕获组 + 新值」替换，保留缩进与注释）
    sub("Dockerfile", r"ARG VERSION=v?\d+\.\d+\.\d+", f"ARG VERSION={new}", count=0)
    sub("docker-compose.yml", r"\$\{VERSION:-v?\d+\.\d+\.\d+\}", "${VERSION:-" + new + "}", count=0)
    sub("cmd/server/main.go", r'var Version = "v?\d+\.\d+\.\d+-dev"', f'var Version = "{new}-dev"')
    sub("web/src/pages/SettingsPage.vue",
        r"session\.version \|\| 'v?\d+\.\d+\.\d+'", f"session.version || '{new}'")
    # 侧栏显示的是 vite 注入的「前端构建版本」（web/src/build.ts），由
    # vite.config.ts 构建时读 VERSION 得到，这里没有需要同步的硬编码。
    sub("web/src/layout/AppTopbar.vue",
        r"session\.version \|\| '[^']*'", f"session.version || '{new[1:]}'")
    sub("web/src/pages/LoginPage.vue",
        r"session\.version \|\| '[^']*'", f"session.version || '{new[1:]}'")

    # 3) package.json（保持原有缩进与字段顺序，只改 version）
    pkg_path = ROOT / "web" / "package.json"
    pkg_text = pkg_path.read_text(encoding="utf-8", newline="")
    pkg_json = json.loads(pkg_text)
    pkg_json["version"] = new[1:]
    nl = "\r\n" if "\r\n" in pkg_text else "\n"
    pkg_text_new = json.dumps(pkg_json, ensure_ascii=False, indent=2) + nl
    pkg_path.write_text(pkg_text_new, encoding="utf-8", newline="")
    print("  ✓ web/package.json")

    # 4) 本机 .env 如果存在且写了 VERSION，也跟着走；不入库，不新建。
    env_path = ROOT / ".env"
    if env_path.exists():
        env = env_path.read_text(encoding="utf-8", newline="")
        env_new, n = re.subn(r"(?m)^VERSION=.*$", f"VERSION={new}", env)
        if n:
            env_path.write_text(env_new, encoding="utf-8", newline="")
            print("  ✓ .env（本机）")

    # 5) .env.example 也要同步：新用户 `cp .env.example .env` 之后，
    #    compose 会拿这里的 VERSION 当镜像 tag 和 -X main.Version 的 build arg。
    #    漏掉这一处，界面会显示旧版本、镜像也叫旧名字。
    sub(".env.example", r"(?m)^VERSION=.*$", f"VERSION={new}")

    print(f"\n完成。记得把 VERSION 一起提交；重建容器用：docker compose up -d --build strm115")


if __name__ == "__main__":
    main()
