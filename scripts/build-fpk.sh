#!/bin/bash
#
# GitHub++ fpk 构建脚本。
#
# 依赖：
#   - go 1.24+      （交叉编译 linux 二进制）
#   - fnpack        （飞牛官方打包工具，https://developer.fnnas.com/）
#
# 用法：
#   ./scripts/build-fpk.sh            # 构建 x86 与 arm64 两个 fpk
#   ./scripts/build-fpk.sh x86        # 仅构建 x86
#   ./scripts/build-fpk.sh arm        # 仅构建 arm64
#
# 产物输出到 dist/ 目录。

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

VERSION="${FPK_VERSION:-1.0.2}"
DIST="${ROOT}/dist"
mkdir -p "${DIST}"

LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${FPK_COMMIT:-local} -X main.buildTime=$(date +%Y-%m-%dT%H:%M:%S) -X github.com/ghpp/ghpp/internal/api.Version=${VERSION}"

build_one() {
    local platform="$1"   # x86 | arm
    local goarch="$2"     # amd64 | arm64
    # 注意：workdir 必须用相对路径。
    # go.exe 是原生 Windows 程序，在某些 bash 环境下不会把 /d/... 的
    # POSIX 路径转换成 Windows 路径，绝对路径会让二进制被写到
    # "D:\d\CODE\..." 这种错误位置，fpk 里就没有主程序了。
    # 相对路径（脚本已 cd 到仓库根目录）不依赖路径转换，永远安全。
    local workdir=".build/fpk-${platform}"

    echo "==> 构建 ${platform} (${goarch}) 版本 ${VERSION}"

    # 1. 组装打包目录：fpk/ 是工程模板，app/ 下的二进制按架构放入。
    #
    # 注意：这里不能 rm -rf 清空工作目录 —— 部分环境的 rm 带批量删除保护
    # （按"每个回合的删除总数"计数），删多了会直接中断脚本。
    # 因此只删除真正可能残留的旧文件，其余交给 cp -r 覆盖：
    #   - manifest/manifest.bak：会被 sed 改写过，必须重建
    #   - *.fpk：上次打包的产物，留着会让收集步骤取错文件
    #   - app/ui/config：内容会被改版，必须覆盖而不是合并
    #   - app/ghpp：旧架构的二进制，绝不能混进新包
    # 图标文件名与 fpk/ 模板一致，cp -r 会直接覆盖，无需单独删除。
    mkdir -p "${workdir}/app/ui/images"
    rm -f "${workdir}/manifest" "${workdir}/manifest.bak" "${workdir}"/*.fpk
    rm -f "${workdir}/app/ghpp" "${workdir}/app/ui/config"
    cp -r "${ROOT}/fpk/." "${workdir}/"

    # 包根目录的 ICON.PNG / ICON_256.PNG 由 fpk/ 内的副本提供（见仓库 fpk/ 目录）。

    # 2. 更新 manifest 中的版本号与平台。
    sed -i.bak "s/^version=.*/version=${VERSION}/; s/^platform=.*/platform=${platform}/" "${workdir}/manifest"
    rm -f "${workdir}/manifest.bak"

    # 3. 交叉编译。
    echo "    GOOS=linux GOARCH=${goarch} go build ..."
    GOOS=linux GOARCH=${goarch} CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" \
        -o "${workdir}/app/ghpp" ./cmd/ghpp

    # 产物守卫：主程序缺失说明编译或路径出了问题，绝不能继续打包，
    # 否则会产出一个"装得上但永远起不来"的空壳包。
    if [ ! -f "${workdir}/app/ghpp" ]; then
        echo "错误：编译产物 ${workdir}/app/ghpp 不存在，中止打包" >&2
        exit 1
    fi
    echo "    主程序大小：$(du -h "${workdir}/app/ghpp" | cut -f1)"

    # 4. 刷新图标（512/256/64 与 favicon）。
    echo "    生成图标..."
    go run scripts/icon.go >/dev/null

    # 5. 打包。
    echo "    fnpack build ..."
    (cd "${workdir}" && fnpack build -d .)

    # 6. 收集产物：按平台与版本命名，避免 x86 / arm 互相覆盖。
    local fpk_file
    fpk_file="$(find "${workdir}" -maxdepth 1 -name '*.fpk' -print -quit)"
    if [ -n "${fpk_file}" ]; then
        mv "${fpk_file}" "${DIST}/github-plus-plus_${VERSION}_${platform}.fpk"
        echo "    产物：dist/github-plus-plus_${VERSION}_${platform}.fpk"
    else
        echo "    警告：未找到 fpk 产物" >&2
        return 1
    fi
}

TARGET="${1:-all}"

case "${TARGET}" in
x86)
    build_one x86 amd64
    ;;
arm)
    build_one arm arm64
    ;;
all)
    build_one x86 amd64
    build_one arm arm64
    ;;
*)
    echo "未知目标: ${TARGET}（可选 x86 / arm / all）" >&2
    exit 1
    ;;
esac

echo "==> 构建完成"
