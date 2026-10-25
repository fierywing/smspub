// smspub 主程序：加载配置 → 组装 hub（各卡模块生命周期）→ HTTP 服务（REST API + 嵌入前端）。
//
// flag：
//
//	-c 配置文件路径（默认 ./config.json）
//	-listen 覆盖 HTTP 监听地址（优先于配置文件；配置文件里的 listen 变更需重启生效）
//	-https-listen / -tls-cert / -tls-key 同时设置可额外开启 HTTPS
//	-dev 从磁盘 ./web 目录提供前端文件（前端调试用；默认用编译时嵌入的 smspub.WebFS）
//
// RESET 管理员指令 / POST /api/reboot 通过 syscall.Exec 以原始 os.Args 重新拉起自身，
// flag 参数自然继承，无需额外处理。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"smspub/internal/hub"
	"smspub/internal/logger"
	"smspub/internal/web"
)

func main() {
	cfgPath := flag.String("c", "config.json", "配置文件路径")
	listen := flag.String("listen", "", "覆盖监听地址，如 :8080")
	httpsListen := flag.String("https-listen", "", "额外的 HTTPS 监听地址，如 :8444（需证书）")
	tlsCert := flag.String("tls-cert", "", "HTTPS PEM 证书文件")
	tlsKey := flag.String("tls-key", "", "HTTPS PEM 私钥文件")
	dev := flag.Bool("dev", false, "从磁盘 ./web 目录提供前端静态文件（调试用）")
	flag.Parse()

	lg := logger.New(1024)
	h, err := hub.New(*cfgPath, lg)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	addr := *listen
	if addr == "" {
		addr = h.Config().Listen
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hubDone := make(chan struct{})
	var handler http.Handler = web.New(h, lg)
	if *dev {
		api := handler
		files := http.FileServerFS(os.DirFS("web"))
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
				api.ServeHTTP(w, r)
				return
			}
			files.ServeHTTP(w, r)
		})
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	secure, err := httpsServer(*httpsListen, *tlsCert, *tlsKey, handler)
	if err != nil {
		log.Fatalf("HTTPS 初始化失败: %v", err)
	}
	servers := []*http.Server{srv}
	if secure != nil {
		servers = append(servers, secure)
	}
	go func() { defer close(hubDone); h.Start(ctx) }()
	for _, server := range servers {
		protocol := "HTTP"
		if server.TLSConfig != nil {
			protocol = "HTTPS"
		}
		lg.Infof("main", "%s 服务启动于 %s（-dev=%v）", protocol, server.Addr, *dev)
		go func(server *http.Server, protocol string) {
			var err error
			if server.TLSConfig != nil {
				err = server.ListenAndServeTLS("", "")
			} else {
				err = server.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				lg.Errorf("main", "%s 服务异常退出: %v", protocol, err)
				os.Exit(1)
			}
		}(server, protocol)
	}

	<-ctx.Done()
	lg.Infof("main", "收到退出信号，开始优雅停机")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			lg.Warnf("main", "%s 停机超时: %v", server.Addr, err)
		}
	}
	<-hubDone // hub.Start 收到同一 ctx 取消后停止全部卡片再返回
	lg.Infof("main", "已退出")
}
