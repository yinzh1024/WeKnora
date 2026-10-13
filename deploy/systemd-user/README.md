# WeKnora Lite 用户级 systemd 服务

把 **WeKnora Lite** 与 **DocReader** 解析服务托管为 systemd **用户服务**（`systemctl --user`），
支持开机自启（无需登录，依赖 `linger`）。

## 目录内容

| 文件 | 作用 |
|------|------|
| `weknora.service` | WeKnora Lite 主服务（HTTP，默认 8080） |
| `weknora-docreader.service` | DocReader gRPC 解析服务（默认 50051） |
| `README.md` | 本文件 |

## 与 `deploy/weknora-lite.service` 的区别

- `deploy/weknora-lite.service`：**系统级**服务（`multi-user.target`，`User=weknora`，`/opt/weknora`），随发行包分发。
- 本目录：**用户级**服务（`default.target`，`systemctl --user`），适合在源码目录本机部署；并额外包含 DocReader。

## 前置条件

1. 已构建 Lite 单二进制 `./WeKnora-lite`（见仓库根 `build_lite.sh`）
2. 已安装 DocReader 依赖：`cd docreader && uv sync`
3. 已创建 `.env.lite`（`cp .env.lite.example .env.lite`）

> **路径**：unit 内的工作目录固定为 `/media/yinzh/ext/WeKnora`。若部署路径不同，安装前先替换：
> ```bash
> sed -i "s|/media/yinzh/ext/WeKnora|$PWD|g" deploy/systemd-user/*.service
> ```

## 安装 / 启用

```bash
mkdir -p ~/.config/systemd/user
cp deploy/systemd-user/*.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now weknora-docreader.service weknora.service
loginctl enable-linger "$USER"   # 开机 / 未登录时也运行
```

## 常用管理命令

```bash
# 状态
systemctl --user status weknora
systemctl --user status weknora-docreader

# 启动 / 停止 / 重启
systemctl --user start   weknora            # 会一并带起 docreader（Wants）
systemctl --user stop    weknora
systemctl --user restart weknora
systemctl --user stop    weknora weknora-docreader

# 开机自启
systemctl --user enable  weknora weknora-docreader
systemctl --user disable weknora weknora-docreader
systemctl --user is-enabled weknora weknora-docreader
loginctl show-user "$USER" -p Linger        # 确认 linger=yes

# 日志
journalctl --user -u weknora -f
journalctl --user -u weknora-docreader -f
tail -f data/lite.log data/docreader.log    # unit 同时 append 到文件

# 排障
systemctl --user reset-failed weknora weknora-docreader
systemctl --user daemon-reload              # 修改 unit 后
```

## 行为说明

- `weknora.service` 通过 `Wants=weknora-docreader.service` 在启动时自动拉起 docreader；
  但 `stop weknora` **不会**停 docreader。若需联动停止，把该依赖改为
  `Requires=weknora-docreader.service` 并在 docreader 单元加 `PartOf=weknora.service`。
- 两个服务均为 `Restart=on-failure`，异常退出后 3 秒自动重启。
- 日志同时 append 到 `data/lite.log` 与 `data/docreader.log`。
- DocReader 是 Lite `builtin` 解析（PDF/Office）的必需依赖：Lite 单二进制只含 gRPC 客户端，
  不含服务端，须由 `weknora-docreader.service` 提供。
