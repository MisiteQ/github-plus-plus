// Command ghpp 是运行在飞牛 NAS 上的 GitHub 加速器。
//
// 两个监听端口分工明确：
//
//	7710 —— 加速代理入口，客户端把请求指向这里
//	7717 —— Web 控制台，用于管理与查看状态
//
// 典型用法：
//
//	ghpp                    # 使用默认数据目录启动
//	ghpp -data /vol1/ghpp   # 指定数据目录
//	ghpp -version           # 查看版本
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ghpp/ghpp/internal/api"
	"github.com/ghpp/ghpp/internal/app"
	"github.com/ghpp/ghpp/internal/config"
	"github.com/ghpp/ghpp/internal/hostsfile"
	"github.com/ghpp/ghpp/internal/logbus"
	"github.com/ghpp/ghpp/web"
)

// 版本信息由构建脚本通过 -ldflags 注入。
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	var (
		dataDir   = flag.String("data", "", "数据目录，默认为飞牛应用数据目录或 ~/.ghpp")
		proxyAddr = flag.String("proxy", "", "覆盖代理监听地址，例如 0.0.0.0:7710")
		webAddr   = flag.String("web", "", "覆盖控制台监听地址，例如 0.0.0.0:7717")
		showVer   = flag.Bool("version", false, "打印版本信息后退出")
		resetPass = flag.Bool("reset-password", false, "重置控制台密码后退出")
		clearHost = flag.Bool("clear-hosts", false, "清理系统 hosts 中的加速托管区后退出，供卸载时调用")
		noWeb     = flag.Bool("no-web", false, "仅运行代理，不启动控制台")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("GitHub++ %s (commit %s, built %s)\n", version, commit, buildTime)
		return
	}

	// 解析数据目录。
	dir := config.ResolveDataDir(*dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建数据目录 %s 失败: %v\n", dir, err)
		os.Exit(1)
	}

	// 初始化日志总线：内存保留 2000 条，同时落盘到数据目录。
	logPath := ""
	if etcDir := config.ResolveEtcDir(dir); etcDir != "" {
		logPath = etcDir + string(os.PathSeparator) + "ghpp.log"
	}
	log := logbus.New(2000, logPath)
	defer log.Close()

	// 加载配置。
	cfg, err := config.Load(dir)
	if err != nil {
		log.Logf("error", "加载配置失败: %v", err)
		os.Exit(1)
	}

	// 命令行参数覆盖配置。
	if *proxyAddr != "" {
		_ = cfg.Update(func(c *config.Config) error {
			c.Proxy.Listen = *proxyAddr
			return nil
		})
	}
	if *webAddr != "" {
		_ = cfg.Update(func(c *config.Config) error {
			c.Server.Listen = *webAddr
			return nil
		})
	}

	// 密码重置：用于忘记密码时通过命令行恢复访问。
	if *resetPass {
		newPass := config.RandomToken(8)
		if err := cfg.Update(func(c *config.Config) error {
			c.Server.Password = newPass
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "重置密码失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("新的控制台密码: %s\n", newPass)
		fmt.Printf("用户名: %s\n", cfg.Server.Username)
		return
	}

	// hosts 清理：卸载应用时由生命周期脚本调用，
	// 即使服务没有正常停止过也能把系统 hosts 还原到初始状态。
	if *clearHost {
		mgr := hostsfile.NewManager(cfg.Hosts.FilePath, func(string, string, ...any) {})
		if err := mgr.Clear(); err != nil {
			fmt.Fprintf(os.Stderr, "清理 hosts 失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已清理 %s 中的加速托管区\n", cfg.Hosts.FilePath)
		return
	}

	cfg = cfg.Snapshot()

	// 打印启动横幅，把关键信息清楚地展示给用户。
	printBanner(cfg, dir, log.Path())

	// 组装应用。
	application := app.New(cfg, log)

	if err := application.Start(); err != nil {
		log.Logf("error", "启动失败: %v", err)
		os.Exit(1)
	}

	// 控制台服务。
	var server *api.Server
	if !*noWeb {
		api.Version = version
		server = api.NewServer(application, web.Handler())
		go func() {
			if err := server.Start(cfg.Server.Listen); err != nil {
				log.Logf("error", "控制台服务退出: %v", err)
			}
		}()
	}

	// 等待退出信号。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	log.Logf("info", "收到信号 %s，开始优雅退出…", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if server != nil {
		_ = server.Stop(ctx)
	}
	if err := application.Stop(ctx); err != nil {
		log.Logf("warn", "停止服务时出现错误: %v", err)
	}

	// 给日志刷盘留一点时间。
	time.Sleep(200 * time.Millisecond)
}

// printBanner 输出启动信息，包含用户最需要的访问地址与初始密码。
func printBanner(cfg *config.Config, dataDir, logPath string) {
	lanIP := detectLANIP()

	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │              GitHub++ 飞牛 NAS 加速器                    │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Printf("   版本      : %s\n", version)
	fmt.Printf("   数据目录  : %s\n", dataDir)
	fmt.Printf("   配置文件  : %s\n", cfg.Path())
	if logPath != "" {
		fmt.Printf("   日志文件  : %s\n", logPath)
	}
	fmt.Println()
	fmt.Println("   ── 加速代理入口（客户端指向这里）──")
	fmt.Printf("   本机      : http://127.0.0.1:%s\n", portOf(cfg.Proxy.Listen))
	if lanIP != "" {
		fmt.Printf("   局域网    : http://%s:%s\n", lanIP, portOf(cfg.Proxy.Listen))
	}
	fmt.Println()
	fmt.Println("   ── Web 控制台（在这里管理）──")
	fmt.Printf("   本机      : http://127.0.0.1:%s\n", portOf(cfg.Server.Listen))
	if lanIP != "" {
		fmt.Printf("   局域网    : http://%s:%s\n", lanIP, portOf(cfg.Server.Listen))
	}
	fmt.Printf("   用户名    : %s\n", cfg.Server.Username)
	fmt.Printf("   密码      : %s\n", cfg.Server.Password)
	fmt.Println()
	fmt.Println("   提示：请登录控制台后立即修改默认密码。")
	fmt.Println()
}

// portOf 从监听地址中取出端口部分。
func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}

// detectLANIP 探测本机在局域网中的可用 IP，用于打印访问地址。
func detectLANIP() string {
	ifaces, err := netInterfaces()
	if err != nil {
		return ""
	}
	for _, ip := range ifaces {
		if ip == "" || ip == "127.0.0.1" || ip == "::1" {
			continue
		}
		// 跳过回环与链路本地地址。
		if len(ip) >= 4 && ip[:4] == "169." {
			continue
		}
		return ip
	}
	return ""
}
