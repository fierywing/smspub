// Package web 实现 REST API 与嵌入前端静态文件服务。
//
// 路由（Go 1.22 http.ServeMux）：
//
//	GET  /api/cards               → model.Card[]（每卡含最近 5 条短信，最新在前）
//	GET  /api/history             → model.HistMessage[]（全库历史，最新在前，空为 []）
//	POST /api/history/delete      body {"ids":["..."]} → {"ok":true}；ids 空/缺失或超 10000 条 → 400
//	GET  /api/config              → model.Config
//	POST /api/config              body model.Config → {"ok":true} / {"ok":false,"error":"..."}
//	GET  /api/config/channels     → model.Channel[]
//	POST /api/cards/{id}/sms      body {"to","text"} → {"ok":true} / {"ok":false,"error":"..."}
//	POST /api/cards/{id}/ping     → {"ok":true,"ip","latency","ttl","rssi"}（rssi 取卡片缓存值）
//	POST /api/cards/{id}/restart  → {"ok":true}
//	POST /api/at/{id}             body {"cmd":"ATI"} → {"ok":true,"output":"..."}
//	GET  /api/logs?n=200          → logger.Entry[]（时间升序；n 缺省 200，n<=0 表示全部）
//	POST /api/reboot              → {"ok":true}，随后 hub.Reboot() 重启进程
//	/                             嵌入前端静态文件（smspub.WebFS 的 web/ 子树）
//
// 防 CSRF：写方法（POST/PUT/DELETE/PATCH）携带 Origin 且与目标 Host 不同源 → 403
// （withCSRF 中间件）；全部写端点还要求 Content-Type 为 application/json——带体端点
// 经 decode、无体的 ping/restart/reboot 经 requireJSON，跨站表单无法伪造该头（415）。
// 静态文件不含敏感数据，所有敏感操作都经 /api，故静态路径不做额外校验。
//
// New(h, lg) 返回可直接交给 http.Server 的 Handler；监听地址由 main 从配置读取，
// 本包不监听端口。依赖最小化：只需 hub（配置与全部卡片操作都经它）与 logger（/api/logs）。
package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"strings"
	"sync"

	"smspub"
	"smspub/internal/hub"
	"smspub/internal/logger"
	"smspub/internal/voice"
)

// maxBodySize 请求体大小上限（1MiB）：配置/短信/AT 命令都远小于此值，防御性兜底。
const maxBodySize int64 = 1 << 20

// logsDefaultN /api/logs 未带 n 参数时的默认条数。
const logsDefaultN = 200

// Server web 层服务器：持有 hub 与 logger，mux 承载全部路由。
type Server struct {
	hub *hub.Hub
	lg  *logger.Logger
	mux *http.ServeMux

	// reboot 进程重启动作，默认 hub.Reboot（syscall.Exec 自我替换）；
	// 独立成变量仅为测试注入 no-op，避免测试进程被替换。
	reboot          func()
	resolveAudio    func(string) (voice.Device, error)
	openAudio       func(string) (audioStream, error)
	openMicrophone  func(context.Context, string, voice.ATQuery) (microphoneStream, error)
	microphoneMu    sync.Mutex
	microphones     map[string]microphoneSession
	callAudioActive func(string) bool
}

// New 构造完整 HTTP 处理器：/api/* 提供全部 REST 端点，/ 提供嵌入前端静态文件，
// panic 由 recover 中间件兜底为 500 JSON。
func New(h *hub.Hub, lg *logger.Logger) http.Handler {
	return newServer(h, lg).handler()
}

// newServer 组装 Server 并注册路由（测试可直接取内部结构注入 panic 路由 / reboot）。
func newServer(h *hub.Hub, lg *logger.Logger) *Server {
	if lg == nil {
		lg = logger.New(1024)
	}
	s := &Server{hub: h, lg: lg, mux: http.NewServeMux(), reboot: h.Reboot}
	s.resolveAudio = voice.Resolve
	s.openAudio = func(device string) (audioStream, error) { return voice.Open(device) }
	s.openMicrophone = func(ctx context.Context, device string, query voice.ATQuery) (microphoneStream, error) {
		return voice.OpenDuplex(ctx, device, query)
	}
	s.microphones = make(map[string]microphoneSession)
	s.callAudioActive = s.hasActiveCall
	s.routes()
	return s
}

// handler 组合 panic 兜底、安全响应头、CSRF 防护与全部路由。
// recover 最外层保证安全头/CSRF 判定本身的异常也能兜底为 500 JSON。
func (s *Server) handler() http.Handler {
	return s.withRecover(s.withSecurityHeaders(s.withCSRF(s.mux)))
}

// withSecurityHeaders 为全部响应加 Content-Security-Policy：前端无内联脚本、无
// 外链资源（脚本/连接来源锁死本站），作为 XSS 纵深防御。例外：img-src 放开
// data:（页面 favicon 为 data: URI 的 SVG）；style-src 放开 unsafe-inline（渲染
// 模板使用内联 style 属性，样式被 <style> 与 style.css 承载）。
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

// routes 注册全部端点。Go 1.22 方法感知路由：未注册的方法组合由 mux 自动返回 405。
func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /api/cards", s.handleCards)
	m.HandleFunc("GET /api/history", s.handleHistory)
	m.HandleFunc("POST /api/history/delete", s.handleHistoryDelete)
	m.HandleFunc("GET /api/config", s.handleGetConfig)
	m.HandleFunc("POST /api/config", s.handlePostConfig)
	m.HandleFunc("GET /api/config/channels", s.handleChannels)
	m.HandleFunc("GET /api/serialports", s.handleSerialPorts)
	m.HandleFunc("POST /api/cards/{id}/sms", s.handleSMS)
	m.HandleFunc("POST /api/cards/{id}/call", s.handleCall)
	m.HandleFunc("GET /api/cards/{id}/audio/status", s.handleAudioStatus)
	m.HandleFunc("GET /api/cards/{id}/audio", s.handleCallAudio)
	m.HandleFunc("POST /api/cards/{id}/audio/microphone", s.handleMicrophoneChunk)
	m.HandleFunc("POST /api/cards/{id}/ping", s.handlePing)
	m.HandleFunc("POST /api/cards/{id}/restart", s.handleRestart)
	m.HandleFunc("POST /api/at/{id}", s.handleAT)
	m.HandleFunc("GET /api/logs", s.handleLogs)
	m.HandleFunc("POST /api/reboot", s.handleReboot)
	// /api 兜底：未匹配的路径/方法也返回 JSON 404，避免落入静态文件服务返回 HTML 404
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "接口不存在")
	})
	m.Handle("/", newStatic())
}

// withRecover panic 兜底中间件：任何 handler 的 panic 转为 500 JSON，进程不退出。
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.lg.Errorf("http", "panic 已恢复 %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				writeErr(w, http.StatusInternalServerError, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withCSRF 跨站请求防护中间件：恶意网页可借浏览器对本地服务的跨站请求伪造
// 写操作（重启、发短信、改配置等）。对写方法（POST/PUT/DELETE/PATCH），若
// 请求携带 Origin 头且其 host:port 与目标 r.Host 不一致 → 403。
// 无 Origin 头的请求（curl、跨源重定向后的请求等）放行；浏览器同源 fetch 虽也
// 带 Origin，但其 host:port 与 r.Host 一致，同样放行。GET/HEAD 无副作用，不校验。
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && isWriteMethod(r.Method) {
			if !sameOriginHost(origin, r.Host) {
				s.lg.Warnf("http", "已拦截跨站请求 %s %s（Origin %q 与目标 %q 不同源）",
					r.Method, r.URL.Path, origin, r.Host)
				writeErr(w, http.StatusForbidden, "已拒绝跨站请求")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isWriteMethod 返回是否为可能改状态的 HTTP 方法（CSRF 防护对象）。
func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// sameOriginHost 判断 Origin 的 host:port 与请求目标 r.Host 是否同源。
//
// 端口比对规则：浏览器对默认端口一律省略（页面 https://x 时 Origin 为
// "https://x"），但经反向代理等场景 r.Host 可能显式带上默认端口（"x:443"）。
// 故把 Origin 按其 scheme 补全默认端口（http→80、https→443）后比对；r.Host
// 省略端口时按浏览器语义视为默认端口（与 80/443 等价）。Origin 带非默认端口
// 而 Host 省略（或相反）只可能是伪造或异常代理，宁严勿漏判为不同源。
//
// 其他规则：主机名不区分大小写；IPv6 字面量容忍带/不带方括号；Origin 解析
// 失败或非 http(s) scheme（如沙箱 iframe 的 "null"）一律判为不同源。
func sameOriginHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	oHost := strings.ToLower(u.Hostname())
	oPort := u.Port()
	if oPort == "" {
		if u.Scheme == "http" {
			oPort = "80"
		} else {
			oPort = "443"
		}
	}
	tHost, tPort, err := net.SplitHostPort(host)
	if err != nil {
		tHost, tPort = host, "" // 无端口形式
	}
	tHost = strings.ToLower(strings.Trim(tHost, "[]")) // 去掉 IPv6 字面量的方括号
	if oHost != tHost {
		return false
	}
	if tPort == "" {
		return oPort == "80" || oPort == "443" // Host 省略端口 = 默认端口
	}
	return oPort == tPort
}

// newStatic 嵌入前端静态文件服务：smspub.WebFS 的 web/ 子树。
// 静态资源不含任何敏感数据，敏感操作全部经 /api，故此处不做额外校验。
//
// embed 文件无 mtime，http.FileServerFS 的响应既无 Last-Modified 也无 ETag，
// 浏览器只能 heuristic 缓存，升级二进制后可能继续用旧 app.js/history.js。故为
// 每个文件预计算内容哈希 ETag（embed 后文件集与内容固定，启动时一次遍历即可），
// 响应带 ETag 与 Cache-Control: no-cache（可缓存但每次回源协商），If-None-Match
// 命中则返回 304，协商未命中才传完整内容——发版后浏览器总能拿到新文件。
func newStatic() http.Handler {
	sub, err := fs.Sub(smspub.WebFS, "web")
	if err != nil {
		// 嵌入目录名固定为 "web"，理论不可达；兜底为全部 404
		return http.NotFoundHandler()
	}
	return &staticHandler{inner: http.FileServerFS(sub), etags: buildETags(sub)}
}

// buildETags 遍历静态文件树，构建 相对路径 → ETag 映射。
// ETag 取文件内容 SHA-256 前 16 字节的十六进制并加引号（RFC 7232 强 ETag 形态）。
// embed 内容不可变，启动时计算一次即可；个别文件读取失败仅跳过（退化为无
// ETag 协商），不影响其余文件与其他静态行为。
func buildETags(fsys fs.FS) map[string]string {
	etags := make(map[string]string)
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil // 目录继续下钻，本身不进表
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		etags[p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return etags
}

// staticHandler 在 http.FileServerFS 之上补充 ETag / If-None-Match 协商缓存。
// etags 构造后只读，并发访问安全。
type staticHandler struct {
	inner http.Handler
	etags map[string]string // web/ 子树内相对路径 → ETag
}

// ServeHTTP 命中文件时先比对 If-None-Match：匹配返回 304（不带 body，带 ETag
// 与 Cache-Control）；未匹配则设好 ETag 与 Cache-Control: no-cache 后交由
// FileServerFS 输出。未命中映射（目录重定向、目录列表、缺失文件）保持
// FileServerFS 原行为，不加缓存头。
func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.inner.ServeHTTP(w, r)
		return
	}
	etag, ok := h.lookupETag(r.URL.Path)
	if !ok {
		h.inner.ServeHTTP(w, r)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if ifNoneMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified) // 304 不带 body
		return
	}
	h.inner.ServeHTTP(w, r)
}

// lookupETag 按 http.FileServerFS 的路径语义推出目标文件名并查 ETag 表：
// 请求路径以 "/" 结尾（含根 "/"）指向目录下的 index.html，其余去掉开头 "/"
// 即为文件名。查不到（目录本身、缺失文件）返回 false。
func (h *staticHandler) lookupETag(urlPath string) (string, bool) {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" || strings.HasSuffix(urlPath, "/") {
		name = path.Join(name, "index.html")
	}
	etag, ok := h.etags[name]
	return etag, ok
}

// ifNoneMatch 判断 If-None-Match 是否与资源 ETag 匹配（304 判定）：
// 支持逗号分隔的标签列表与通配 "*"。本处 ETag 均为强 ETag 且浏览器原样回传，
// 不处理弱比较（W/ 前缀）。
func ifNoneMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) == etag {
			return true
		}
	}
	return false
}

// okResponse 成功响应 {"ok":true}。
type okResponse struct {
	OK bool `json:"ok"`
}

// errResponse 失败响应 {"ok":false,"error":"中文可读信息"}。
type errResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// writeJSON 以统一 Content-Type 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOK 输出 {"ok":true}。
func writeOK(w http.ResponseWriter, status int) { writeJSON(w, status, okResponse{OK: true}) }

// writeErr 输出 {"ok":false,"error":msg}。
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errResponse{OK: false, Error: msg})
}

// decode 解析 JSON 请求体（限 1MiB）。Content-Type 必须为 application/json
// （允许带 charset 等参数），否则 415——这是 CSRF 的第二道防线：跨站表单只能
// 以 text/plain 等类型提交，而前端 fetch 对带体请求均显式声明 application/json
// （见 web/app.js、web/history.js）。JSON 非法返回 400；失败时均已写响应并返回 false。
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !requireJSON(w, r) {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是合法的 JSON")
		return false
	}
	return true
}

// requireJSON 要求请求 Content-Type 为 application/json，否则 415（decode 的
// 第一段，单独暴露给 ping/restart/reboot 这类无请求体的写端点——它们同样属于
// CSRF 防护对象，跨站表单无法伪造该头；前端 fetch 调用时显式声明该头）。
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type 必须为 application/json")
		return false
	}
	return true
}

// fail 把 hub 返回的错误转为统一 JSON 响应：未知卡片固定 404，其余按 500。
func fail(w http.ResponseWriter, err error) {
	if isCardNotFound(err) {
		writeErr(w, http.StatusNotFound, "卡片不存在")
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// isCardNotFound 判断错误是否为未知卡片（hub 对未知卡统一返回该文本）。
func isCardNotFound(err error) bool { return err != nil && err.Error() == "卡片不存在" }
