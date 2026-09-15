# GitHub++

给飞牛 NAS（fnOS）用的 GitHub 加速器。

国内访问 GitHub 是什么体验大家都懂：网页转圈、clone 超时、Release 下到一半断掉。家里的 NAS 明明闲着，不如让它当个加速跳板——这就是这个项目的由来。

装上之后，NAS 会在局域网里开两个端口：**7710** 当代理，**7717** 是网页控制台。家里所有设备直接用，不用每台机器折腾。

## 它能干什么

- **Git 加速**：clone / pull 随便来，不用改 hosts 也不用碰代理软件
- **网页加速**：浏览器里直接逛 GitHub，头像、图片都能正常加载
- **Release / raw 文件下载**：大文件也稳
- **Docker 拉取加速**：Docker Hub 拉镜像不再卡在 Waiting
- **网页控制台**：哪个源快一目了然，实时日志、改配置，点点鼠标就行

原理一句话：内置了十几个社区公益加速通道（ghfast.top、gh-proxy.com 这类），程序会不停测速，谁快用谁，某个源抽风了就自动冷却换下一个。对 API 这类必须直连的域名做了熔断——直连被掐就立刻走镜像，缓过来再切回去，整个过程你无感。

装完之后的维护成本：基本为零。

## 安装

1. 去 [Releases](https://github.com/MisiteQ/github-plus-plus/releases) 下载 fpk 安装包：x86 的 NAS 下 `*_x86.fpk`，ARM 的下 `*_arm.fpk`
2. 飞牛应用中心手动安装
3. 桌面点开 GitHub++ 图标，账号 `admin`，密码 `admin123`（进去后记得改）

## 怎么用

把下面的 `192.168.31.205` 换成你 NAS 的 IP：

```bash
# git clone
git clone http://192.168.31.205:7710/https://github.com/git/git.git

# Release 文件
curl -LO "http://192.168.31.205:7710/https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64"

# raw 文件
curl -LO "http://192.168.31.205:7710/https://raw.githubusercontent.com/git/git/master/README.md"
```

浏览器加速：控制台里开 hosts 模式就行，全局生效不用装证书；想更彻底可以开 MITM 模式，但需要装一次根证书。

Docker 加速：控制台里有说明，把 Docker 的 registry-mirrors 指向 NAS 就行。

## 一个坑

开发时踩过最隐蔽的坑：运营商对 GitHub 不是一刀切封死，而是"放行响应头、掐响应体"——前几 KB 秒开，接着限速到十几 KB/s。看起来像程序 bug，其实是链路被 QoS 了。

所以代理里做了个 512KB 的吞吐探针：往浏览器写数据之前，先验一口上游的真实速度，不达标马上换镜像源。另外还有直连熔断、半开恢复、启动预热这一套，细节在 [internal/proxy/engine.go](internal/proxy/engine.go)，欢迎围观。

## 从源码构建

需要 Go 1.22+ 和飞牛的 fnpack：

```bash
./scripts/build.sh           # 本机构建
./scripts/build-fpk.sh x86   # 交叉编译 + 打包 fpk
./scripts/build-fpk.sh arm
```

产物在 `dist/` 目录。

## 目录结构

```
cmd/ghpp/        程序入口
internal/        核心：反代引擎、镜像池、DNS 优选、控制台 API 等
web/             控制台前端（单页）
fpk/             fnOS 打包模板
scripts/         构建与打包脚本
```

## 最后

- 首次登录记得改密码
- MITM 模式的根证书只装在自己的设备上，别外传
- License: MIT

本项目是本人用 AI 搓出来的——把想法提出来，交给 AI 写，我就负责点确定，然后让它提交、发布，哈哈哈。连上面那个限速的坑，都是让它自己 SSH 上 NAS 一层层抓包查出来的。
