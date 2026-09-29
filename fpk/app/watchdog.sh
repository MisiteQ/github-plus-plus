#!/bin/bash
#
# GitHub++ 看门狗：在 setsid 内被 cmd/main 启动，负责监控 Go 主进程并在异常退出时拉起。
#
# 设计要点：
#   - 由 cmd/main 用 `setsid bash watchdog.sh ... &` 启动，setsid 让本脚本脱离控制终端、
#     进程组与 cgroup，进入新会话；这样关闭桌面窗口 / 登出飞牛账号时，启动 shell 所在
#     会话或 cgroup 被销毁，也不会牵连本脚本与 Go 服务进程退出。
#   - 真实服务进程的 PID 由 Go 程序在启动早期通过 GHPH_PID_FILE 写入，main 的 stop/status
#     据此管理进程，避免依赖 $!（setsid 下 $! 拿到的是 setsid 而非真实服务进程）。
#   - rc=0 视为主动停止（SIGTERM 优雅退出），不再拉起；
#     rc!=0 视为异常崩溃，由 stop_signal / watchdog_enabled 综合判断是否拉起。
#
# 参数（全部从环境变量读，避免转义问题）：
#   BIN                Go 二进制路径
#   TRIM_PKGVAR        数据目录
#   LOG_FILE           日志文件
#   GHPH_PID_FILE      Go 进程要写入的 PID 文件路径
#
# 本脚本不依赖任何外部函数定义，自身完整可执行。

set -u

log_msg() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') - $1" >> "${LOG_FILE}"
}

# watchdog_enabled 读取 ${TRIM_PKGVAR}/watchdog_enabled 标记文件，
# 内容为 "1" 表示启用自动重启，"0" 或文件缺失视为启用（默认 true，与 config 默认值一致）。
watchdog_enabled() {
    local flag_file="${TRIM_PKGVAR}/watchdog_enabled"
    if [ ! -f "${flag_file}" ]; then
        echo 1
        return 0
    fi
    local val
    val=$(head -n 1 "${flag_file}" 2>/dev/null | tr -d '[:space:]')
    if [ -z "${val}" ]; then
        echo 1
        return 0
    fi
    echo "${val}"
}

# should_restart 综合判断是否应该拉起新进程：
#   - stop_signal 文件存在：cmd/main stop 主动停止（含 KILL 路径），不拉起。
#   - watchdog_enabled = "0"：用户在 UI 关闭了自动重启，不拉起。
# 任一满足都不拉起；都通过才返回 0（拉起）。
should_restart() {
    if [ -f "${TRIM_PKGVAR}/stop_signal" ]; then
        return 1
    fi
    [ "$(watchdog_enabled)" = "1" ]
}

log_msg "看门狗启动，准备拉起 Go 主进程"

while true; do
    "${BIN}" -data "${TRIM_PKGVAR}"
    rc=$?
    if [ $rc -eq 0 ]; then
        log_msg "进程主动退出（rc=0），看门狗结束"
        break
    fi
    if ! should_restart; then
        echo "$(date '+%Y-%m-%d %H:%M:%S') - 进程异常退出 rc=${rc}，已不重启（stop 或开关关闭），看门狗结束" >> "${LOG_FILE}"
        break
    fi
    echo "$(date '+%Y-%m-%d %H:%M:%S') - 进程异常退出 rc=${rc}，3 秒后自动重启" >> "${LOG_FILE}"
    sleep 3
done

exit 0
