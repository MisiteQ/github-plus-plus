# GitHub++ 加速器 github-plus-plus

<p>
  <img alt="Version" src="https://img.shields.io/badge/version-1.0.4-blue">
  <img alt="fnOS" src="https://img.shields.io/badge/fnOS-x86%20%7C%20arm64-success">
  <img alt="License" src="https://img.shields.io/badge/license-MIT-orange">
</p>

飞牛 fnOS 上的 GitHub / Docker 加速器（FPK 原生应用）：让闲着的 NAS 当局域网加速跳板，家里所有设备直接用。内置十几个社区公益加速通道，**智能测速自动择优、故障源自动冷却切换**；支持 **Git 克隆 / 网页加速 / Release·raw 下载 / Docker 拉取** 全链路加速。对 API 这类必须直连的域名做了熔断——写客户端前先用 512KB 吞吐探针识别运营商 QoS 限速（"放行响应头、掐响应体"），慢链路自动切镜像，直连恢复后自动切回，全过程无感。Go 单二进制 + 单页前端，**默认不影响 NAS 与局域网其他设备的正常联网**。

- 当前版本：**v1.0.4**
- 作者：**MisiteQ**
- 适用平台：fnOS **x86 + arm64**（最低系统版本 1.0.0）
- 默认端口：**7717**（控制台）、**7710**（代理）
- 默认账号：`admin` / `admin123`（登录后请及时修改）
- 运行身份：**root**（hosts 加速需写 /etc/hosts）

## ✨ 功能

- **🔗 Git 加速**：clone / pull / push 直接走 NAS 中转，不用改 hosts 也不用碰代理软件
- **🌐 网页加速**：浏览器直接逛 GitHub，头像、图片正常加载；hosts 模式全局生效无需装证书，进阶可开 MITM 模式（需装一次根证书）
- **📥 Release / raw 下载**：大文件下载稳定不中断，Release 资产与 raw.githubusercontent.com 文件均可经 NAS 中转
- **🐳 Docker 拉取加速**：Docker Hub 拉镜像不再卡在 Waiting，把 registry-mirrors 指向 NAS 即可
- **📊 Web 控制台**：实时测速看哪个源最快、实时日志、在线改配置、侧边栏可折叠、8 套主题切换
- **🛡 智能调度**：持续测速择优、故障源自动冷却切换；直连快速熔断与半开恢复、启动预热避免重启后首批请求踩慢直连、端到端吞吐兜底熔断

## 📦 安装

### 方式一：FnDepot 应用源（推荐）

在飞牛 fnOS 上安装 [FnDepot](https://github.com/EWEDLCM/FnDepot) 客户端后，添加作者的应用源即可搜索「GitHub++」一键安装 / 升级：

```
https://github.com/MisiteQ/FnDepot
```

### 方式二：手动安装 FPK

1. 到 [Releases](https://github.com/MisiteQ/github-plus-plus/releases) 按 NAS 架构下载：`github-plus-plus_1.0.4_x86.fpk`（x86 机型）或 `github-plus-plus_1.0.4_arm.fpk`（arm64 机型）
2. 飞牛 OS → **应用中心** → 左下角 **手动安装** → 选择 fpk 文件
3. 安装后从桌面打开 **GitHub++**，或直接访问 `http://<NAS_IP>:7717`

> 若「手动安装」入口被关闭，SSH 执行：`appcenter-cli manual-install enable`

## 🚀 使用

把下面的 `192.168.31.205` 换成你 NAS 的 IP：

```bash
# git clone
git clone http://192.168.31.205:7710/https://github.com/git/git.git

# Release 文件
curl -LO "http://192.168.31.205:7710/https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64"

# raw 文件
curl -LO "http://192.168.31.205:7710/https://raw.githubusercontent.com/git/git/master/README.md"
```

浏览器加速：控制台里开 hosts 模式全局生效，无需装证书；想更彻底可开 MITM 模式（需装一次根证书）。

Docker 加速：控制台里有说明，把 Docker 的 registry-mirrors 指向 NAS 即可。

## 🛠 从源码构建

需要 Go 1.24+ 和 [fnpack](https://developer.fnnas.com/docs/cli/fnpack/) 命令行工具：

```bash
go build -o dist/ghpp ./cmd/ghpp   # 本机构建
./scripts/build-fpk.sh x86          # 交叉编译 + 打包 fpk
./scripts/build-fpk.sh arm
```

产物在 `dist/` 目录。

## 📁 项目结构

```
manifest                飞牛应用清单（版本、显示名、端口、权限、更新日志）
fpk/                    fnOS 打包模板与生命周期脚本（安装回调、启动、卸载清理等）
cmd/ghpp/               程序入口
internal/               核心：反代引擎、镜像池、DNS 优选、熔断器、控制台 API 等
web/                    控制台前端（单页，8 套主题）
scripts/                构建与打包脚本（build-fpk.sh、图标生成等）
config/                 飞牛权限与资源声明（以 root 运行）
```

## 🔒 隐私与安全

- 代理服务仅在局域网监听，不对外网暴露，所有流量仅经 NAS 中转，**不上传任何数据到第三方服务器**
- MITM 模式的根证书仅安装在你自己的设备上，别外传；hosts 模式无需证书即可全局加速
- 加速通道为社区公益服务，程序仅做透明反代，不缓存、不篡改、不注入任何内容

## 🙏 致谢

- 社区公益加速通道提供者（ghfast.top、gh-proxy.com 等）
- 飞牛 fnOS 与 [FnDepot](https://github.com/EWEDLCM/FnDepot)

## 📋 版本历史

| 版本 | 内容 |
|---|---|
| v1.0.4 | **界面适配修复**：侧边栏折叠按钮改为边缘悬浮手柄不再遮挡 Logo；请求分布环形图修正为正圆、圆心数字与标签不再重叠；窄屏下侧边栏自动折叠为图标轨、内容单列；加速源表格列宽优化；静态资源改为 no-cache，升级后界面立即生效 |
| v1.0.3 | **控制台视觉升级**：使用真实应用图标、侧边栏支持折叠、新增 8 套主题切换；静态资源 no-cache 防缓存 |
| v1.0.2 | **直连链路加固**：写客户端前的 512KB 吞吐探针识别 QoS 限速、直连快速熔断与半开恢复、启动预热避免重启后首批请求踩慢直连、端到端吞吐兜底熔断，慢链路自动走镜像通道；控制台默认密码改为 admin123 并修复密码未持久化导致重启后无法登录的问题 |
| v1.0.1 | 修复启动问题：桌面图标命名、启动脚本 PID 管理，并新增架构自检（装错架构包时给出明确提示） |
| v1.0.0 | 首个版本：GitHub 反代加速、DNS 优选 hosts 加速、HTTPS 中间人加速、Docker 拉取加速与 Web 控制台 |

完整日志见 [Releases](https://github.com/MisiteQ/github-plus-plus/releases)。

## 📄 许可证

[MIT License](LICENSE) © 2026 MisiteQ
