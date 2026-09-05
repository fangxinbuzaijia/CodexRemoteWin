# Codex Remote Win

<p align="center">
  <img src="assets/codex-remote-icon-512.png" width="160" alt="Codex Remote Win icon">
</p>

一个面向 Windows Codex Desktop 的轻量远程控制桥接程序。它在电脑上启动本地 Web 服务，让手机或其他设备通过浏览器查看 Codex 任务、发送消息和附件，并进行常用的任务管理操作。

> 本项目不是 OpenAI 官方产品，也不是 Codex Mini 的官方 Windows 版本。

## Windows 客户端

- [直接下载 Windows 客户端 EXE](https://github.com/fangxinbuzaijia/CodexRemoteWin/releases/download/v0.10.4/CodexRemoteWin.exe)
- [查看全部版本和校验文件](https://github.com/fangxinbuzaijia/CodexRemoteWin/releases)
- 仓库中的 `client/CodexRemoteWin.exe` 是与当前公开版本对应的单文件客户端。

## 功能

- 响应式手机和桌面网页界面
- 读取并按项目整理本机 Codex 任务
- 查看任务历史、运行状态和上下文用量
- 向指定任务发送文字消息
- 上传图片、文本、Office 文档、PDF、压缩包等附件
- 新建、重命名、置顶、归档、恢复和停止任务
- 请求去重、发送回执检查和失败自动重试
- 六位配对码和浏览器会话令牌
- 支持 `0.0.0.0` 监听，可由 OpenWrt 上的 FRP 客户端转发
- Windows 托盘运行，无控制台黑框
- 可从托盘菜单设置开机启动
- 单文件 Windows 可执行程序，无运行时依赖

## 系统要求

- Windows 10 或 Windows 11（x64）
- 已安装并登录 Codex Desktop
- Codex Desktop 保持运行且 Windows 桌面未锁定

## 快速使用

1. 从 [Releases](https://github.com/fangxinbuzaijia/CodexRemoteWin/releases) 下载 `CodexRemoteWin.exe`。
2. 运行程序。程序会进入系统托盘，不显示控制台窗口。
3. 打开托盘菜单查看配对码和访问地址。
4. 在手机浏览器访问 `http://电脑局域网IP:8787`。
5. 首次访问输入配对码，之后即可选择任务并发送消息。

默认监听地址为 `0.0.0.0:8787`。可以通过环境变量覆盖：

```powershell
$env:CODEX_REMOTE_HOST = '0.0.0.0'
$env:CODEX_REMOTE_PORT = '8787'
$env:CODEX_REMOTE_PAIR_CODE = '123456'
.\CodexRemoteWin.exe
```

可用变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CODEX_REMOTE_HOST` | `0.0.0.0` | Web 服务监听地址 |
| `CODEX_REMOTE_PORT` | `8787` | Web 服务端口 |
| `CODEX_REMOTE_PAIR_CODE` | 随机六位数 | 固定配对码 |
| `CODEX_REMOTE_CDP_PORT` | 自动探测 | 可选的 Codex CDP 端口 |

## OpenWrt + FRP

当 `frpc` 运行在 OpenWrt 路由器上时，`local_ip` 应填写 Windows 电脑的局域网 IP，而不是 `127.0.0.1`：

```toml
[[proxies]]
name = "codex-remote-win"
type = "tcp"
localIP = "192.168.6.158"
localPort = 8787
remotePort = 18787
```

上面的 IP 和端口只是示例，请替换为自己的网络配置。

## 安全说明

程序默认监听所有网卡，这是为了支持局域网和路由器上的 FRP 客户端。不要把裸露的 `8787` 端口直接映射到公网。

推荐至少采取以下措施：

- 在 FRP 服务端启用 TLS 和身份认证
- 通过 HTTPS 反向代理访问
- 使用防火墙限制来源 IP
- 不要使用简单或长期不变的固定配对码
- 定期删除程序目录下的 `data` 文件夹以清理配对记录和上传附件

浏览器会话令牌保存在浏览器本地存储中，服务端只保存令牌哈希。运行数据默认位于 EXE 同目录的 `data` 文件夹，该目录不会提交到 Git。

## 从源码构建

需要 Go 1.22 或更高版本，并在 Windows x64 环境运行：

```powershell
git clone https://github.com/fangxinbuzaijia/CodexRemoteWin.git
cd CodexRemoteWin
.\build.ps1
```

生成文件位于 `dist/CodexRemoteWin.exe`。构建脚本会自动生成 Windows 图标资源；本地存在 `tools/rsrc.exe` 时直接使用，否则会按固定版本下载并运行资源工具。

## 工作原理

程序读取当前用户目录下的 Codex 会话文件来展示任务和历史。发送纯文本时优先尝试 Codex 的 CDP 页面通道；不可用时，程序会切换到 Windows 窗口聚焦、剪贴板和键盘输入，并通过会话文件变化确认消息是否被接收。

由于兼容路径依赖桌面交互，Windows 锁屏、Codex 窗口关闭或 Codex UI 大幅更新时可能导致发送失败。

## 上游与许可

本项目在产品思路和交互流程上参考了 [CoimgRain/Codex-Mini](https://github.com/CoimgRain/Codex-Mini)，并为 Windows、系统托盘、OpenWrt/FRP 和单文件 EXE 场景重新实现。

依照上游许可要求，本项目采用 [Codex Mini Source-Available Non-Commercial License 1.0](LICENSE)：允许个人、教育、研究、评估等非商业用途；商业使用需要获得原版权所有者的书面授权。
