# GitHub++

<p align="center">
  <img src="fpk/ICON.PNG" width="128" alt="GitHub++ 图标">
</p>

**GitHub++ 加速器** 是一个运行在飞牛 NAS（fnOS）上的 GitHub 加速服务。它在局域网内架起一道加速层：智能测速选择最优通道，自动规避被干扰的直连链路，让网页浏览、git 克隆、Release 下载、Docker 拉取在受限网络下依然顺畅。

> 本项目 **100% 由 AI 开发**——从架构设计、代码实现、Bug 定位到打包发布，详见文末 [关于：纯 AI 开发](#关于纯-ai-开发)。

## 功能特性

### 五大加速能力

| 能力 | 说明 |
|---|---|
| **反向代理加速** | 内置 13 个加速通道（社区公益镜像 + raw CDN + git clone 镜像），运行期持续测速、动态加权调度，单个通道劣化自动冷却换源 |
| **DNS 优选 hosts 加速** | 后台对 GitHub 全系域名做 IP 优选与吞吐测速，将最优 IP 写入 NAS hosts，网页/CLI 全局生效 |
| **HTTPS 中间人加速** | 可选开启 MITM 模式，接管浏览器到 GitHub 的 TLS 流量并重写为加速地址；控制台一键下载安装根证书 |
| **直连智能熔断** | 对 API 等敏感域名优先直连，配备 512KB 吞吐探针 + 快速熔断 + 半开恢复 + 启动预热，被 QoS 限速/掐断的链路自动让位给镜像通道 |
| **Docker 拉取加速** | 内置多个 Docker Hub 镜像上游（1ms、DaoCloud、轩辕），自动故障切换，NAS 上直接拉取官方镜像不再卡死 |

### Web 控制台

- 镜像源管理：增删改查、单源/全量测速、启用开关
- 加速模式切换、DNS 优选状态与 hosts 一键写入
- 实时日志流（SSE）、服务重启、配置持久化
- 根证书下载（MITM 模式用）、Docker 配置指引
- 登录鉴权：默认账号 `admin`，初始密码 `admin123`，登录后请立即修改

## 工作原理

```
NAS 内设备 ──▶ GitHub++ (7710 代理 / 7717 控制台)
                    │
                    ├─ 反代引擎：按域名分类路由（web / raw / clone）
                    │    ├─ 直连优先域名：先直连，512KB 吞吐探针验证
                    │    │   ├─ 通过 ──▶ 直连转发
                    │    │   └─ 失败 ──▶ 熔断 N 分钟，后台半开探测
                    │    └─ 其余域名 ──▶ 镜像池（加权随机 + 冷却换源）
                    ├─ DNS 优选：真实文件吞吐测速 → hosts 劫持
                    ├─ MITM：自签 CA 接管浏览器 TLS → 重写加速
                    └─ Docker：registry 反代 → 多上游容灾
```

直连熔断的关键设计：**在向客户端写出任何字节之前**，先限时读满 512KB 验证上游真实吞吐——专治"响应头秒回、响应体被限速到十几 KB/s"的假活链路；冷却到期后由后台探测用真实大端点复检，通过后仅放行一个真实请求验证，避免误判恢复连累整批请求。

## 快速开始

### 安装

1. 从 [Releases](https://github.com/MisiteQ/github-plus-plus/releases) 下载对应架构的 fpk 安装包：
   - `github-plus-plus_1.0.2_x86.fpk` — x86_64 NAS
   - `github-plus-plus_1.0.2_arm.fpk` — ARM64 NAS
2. 飞牛应用中心手动安装，或 SSH 执行 `appcenter-cli install-fpk <文件名>`
3. 桌面点击 GitHub++ 图标进入控制台，默认账号 `admin` / `admin123`

### 使用

**git 克隆 / Release / raw 文件**（把 NAS IP 换成你的）：

```bash
# git 克隆
git clone http://192.168.31.205:7710/https://github.com/git/git.git

# Release 资产
curl -LO "http://192.168.31.205:7710/https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64"

# raw 文件
curl -LO "http://192.168.31.205:7710/https://raw.githubusercontent.com/git/git/master/README.md"
```

**浏览器加速**：控制台开启 hosts 模式（全局生效），或安装根证书 + 开启 MITM 模式。

**Docker 加速**：控制台查看指引，将 Docker daemon 的 `registry-mirrors` 指向 `http://<NAS-IP>:7710`。

## 端口

| 端口 | 用途 |
|---|---|
| 7710 | 代理端口（反代 / CONNECT 代理 / Docker registry） |
| 7717 | Web 控制台 |

## 从源码构建

依赖：Go 1.22+、飞牛 `fnpack` 打包工具。

```bash
# 本机构建
./scripts/build.sh

# 交叉编译 + 打包 fpk（x86 / arm）
./scripts/build-fpk.sh x86
./scripts/build-fpk.sh arm
```

产物在 `dist/` 下。交叉编译输出 linux/amd64 与 linux/arm64，无 CGO 依赖。

## 项目结构

```
cmd/ghpp/              程序入口
internal/
  proxy/               核心引擎：反代调度、镜像池转发、直连熔断（含单元测试）、
                       CONNECT 隧道、MITM CA、Docker registry 反代
  mirror/              镜像池：测速、加权调度、冷却换源
  decide/              自动决策：综合测速结果选择加速模式与通道
  dnsopt/              DNS 优选：IP 探测、吞吐测速、最优 IP 选举
  hostsfile/           hosts 文件安全写入与回滚
  config/              配置、内置镜像/域名清单
  api/                 控制台 REST API + 静态资源
  logbus/              内存日志总线（SSE 实时推送）
  app/                 应用生命周期
web/                  控制台前端（单页）
fpk/                  fnOS 打包模板（manifest、安装脚本、图标）
scripts/              构建与打包脚本
```

## 安全提示

- 首次登录后请立即在控制台修改默认密码
- MITM 模式涉及自签根证书，仅在可信局域网内使用，证书只安装在自有设备上

## 关于：纯 AI 开发

这个项目从头到尾由 **AI 编写**，人类负责提需求、点确认、用产品：

- **架构与代码**：全部模块由 AI（Trae Code，基于大模型）设计并实现，包括反代引擎、熔断器、DNS 优选、控制台前后端与 fpk 打包体系
- **调试与定位**：真实网络故障（运营商对 GitHub 的 SNI 干扰与 QoS 限速）由 AI 自主 SSH 登录 NAS，逐层抓包复现定位——"响应体被限速"这种隐蔽问题，是通过让 AI 设计 512KB 限时探针对比实验发现的
- **发布运维**：交叉编译、fpk 打包、NAS 部署、GitHub 发布，同样由 AI 自动执行
- 人类的参与仅限于：描述症状（"拒绝了我们的连接请求"）、提供 SSH 密码、决定默认密码改成什么

如果你的需求足够清晰、愿意给 AI 开放执行环境，这样一个功能完整的 NAS 应用可以在数天内从零到发布。

---

MIT License
