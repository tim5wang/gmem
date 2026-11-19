// Package server 实现 `gmem serve` 的本地 HTTP 数据接口（docs/TD.md §9）。
//
// # 分层约束（本包的**结构性不变量**）
//
// 本包只依赖三样东西：
//
//	store.Ports       图/记忆/本体端口（接口，无 SQL）
//	recall.Recaller   召回（q 模式的种子来源）
//	ops.Maintainer    维护面（唯一的写入口：待审裁决）
//
// 它**不得** import gmem/sqlite：SQL 与 schema 细节必须留在后端包内，
// 否则"换后端只换注入"的 P0 契约就作废了。这条约束由 server/guard_test.go
// 的依赖方向守卫钉死（扫 import + 扫写 API）。
//
// # 只读优先（TD §9）
//
// 除 POST /api/review 外**没有任何写路径**。Routes() 把路由表暴露出来，
// guard 用例扫它断言"方法集合"——这样新增一条写路由必须同时改测试，
// 想悄悄加写入口会直接变红。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/tim5wang/gmem/ops"
	"github.com/tim5wang/gmem/recall"
	"github.com/tim5wang/gmem/store"
	"github.com/tim5wang/gmem/web"
)

// Catalog 是**可选**的节点/边枚举端口（概览模式与已失效边补齐需要它）。
//
// 为什么单独定义而不是往 store.Store 里加方法：Store 是 P0 已发布的公开契约，
// 加方法会破坏所有既有实现（含 internal/vtest 的内存后端）。这与 P1 用
// store.OntologyReader 表达"可选扩展"、ops 用 ops.Backend 表达"op 需要但 Port
// 不该带"的手法一致。
//
// 满足它的实现是 sqlite.OpsBackend（签名逐字一致），但本包**不 import** 它：
// 由 cmd/gmem 在装配时注入，依赖方向仍是 cmd → server → store/recall/ops。
type Catalog interface {
	// ListNodes 返回该 scope 的节点（limit<=0 = 全量）。
	ListNodes(ctx context.Context, scope store.Scope, limit int) ([]store.Node, error)
	// ListEdges 返回该 scope 的**当前版本**边（含 t_invalid 非零的已失效边）。
	ListEdges(ctx context.Context, scope store.Scope, limit int) ([]store.Edge, error)
}

// Options 是 Server 的构造参数。
type Options struct {
	Ports    store.Ports
	Recaller *recall.Recaller
	// Maintainer 是唯一写入口的落地者（POST /api/review）。
	Maintainer *ops.Maintainer
	// Catalog 可选：缺省时概览模式报错、已失效边无法补齐（均在 meta.degraded 显式上报）。
	Catalog Catalog
}

// Server 是 HTTP 数据接口的处理器集合。
type Server struct {
	ports    store.Ports
	recaller *recall.Recaller
	maint    *ops.Maintainer
	catalog  Catalog
}

// New 构造 Server。Ports 必须至少有 Store 与 Graph。
func New(opts Options) (*Server, error) {
	if opts.Ports.Store == nil {
		return nil, errors.New("server: nil store port")
	}
	if opts.Ports.Graph == nil {
		return nil, errors.New("server: nil graph port（Graph View 的主体就是图）")
	}
	return &Server{
		ports:    opts.Ports,
		recaller: opts.Recaller,
		maint:    opts.Maintainer,
		catalog:  opts.Catalog,
	}, nil
}

// Route 是一条路由（供守卫用例扫描方法集合）。
type Route struct {
	Method  string
	Pattern string
}

// Routes 返回全部 **API** 路由（JSON 接口）。
//
// 只有 POST /api/review 是写路由：guard 用例要求"写路由集合 == {POST /api/review}"，
// 因此新增任何写入口都必须同时改测试（否则变红）。
//
// 静态资源（GET / 与 GET /assets/*）单独由 StaticRoutes() 申报：它们不是 JSON
// 接口，混进来会让"路由表 = API 契约"这件事变模糊；而"它们也必须只读"由
// 另一个 guard 用例（TestStaticRoutesAreReadOnly）钉死。
func (s *Server) Routes() []Route {
	return []Route{
		{Method: http.MethodGet, Pattern: "/api/graph"},
		{Method: http.MethodGet, Pattern: "/api/entity/"},
		{Method: http.MethodPost, Pattern: "/api/review"},
	}
}

// StaticRoutes 返回内嵌前端的静态路由（只读）。
//
// 与 Routes() 分开的理由：静态资源返回的是 HTML/CSS/JS，不是 JSON 契约；
// 但"只读优先"同样适用，因此单独申报并单独守卫。
func (s *Server) StaticRoutes() []Route {
	return []Route{
		{Method: http.MethodGet, Pattern: "/"},
		{Method: http.MethodGet, Pattern: "/assets/"},
	}
}

// Handler 返回可挂到 net/http 的处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/graph", s.guardMethod(http.MethodGet, s.handleGraph))
	mux.HandleFunc("/api/entity/", s.guardMethod(http.MethodGet, s.handleEntity))
	mux.HandleFunc("/api/review", s.guardMethod(http.MethodPost, s.handleReview))
	// GET /assets/* 返回 //go:embed 进来的 web/dist 产物（只读）。
	mux.HandleFunc("/assets/", s.guardMethod(http.MethodGet, s.handleAsset))
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

// guardMethod 拒绝与路由声明不符的方法（405 而不是悄悄当成 GET 处理）。
func (s *Server) guardMethod(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			s.writeError(w, &apiError{
				status:  http.StatusMethodNotAllowed,
				code:    "method_not_allowed",
				message: fmt.Sprintf("%s 只接受 %s，收到 %s", r.URL.Path, method, r.Method),
			})
			return
		}
		h(w, r)
	}
}

// handleRoot 返回内嵌的 Graph View 页面（GET /）。
//
// 本卡起，GET / **不再**返回"服务说明 JSON"，而是直接给 web/dist/index.html：
// 前端产物已 //go:embed 进二进制，`gmem serve` 起来就能用，不需要另跑前端服务
// （TD §9「运行时依然零外部依赖」）。这是本卡唯一允许替换的既有行为。
//
// 其它路径仍回 JSON 404：视图状态全部在 query string 里，不需要 SPA 回退路由，
// 静默返回 index.html 只会把"URL 打错"伪装成"页面能开"。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.writeError(w, &apiError{
			status: http.StatusMethodNotAllowed, code: "method_not_allowed",
			message: "GET / 只接受 GET",
		})
		return
	}
	if r.URL.Path != "/" {
		s.writeError(w, notFound("路径 "+r.URL.Path+" 不存在"))
		return
	}
	index, err := web.IndexHTML()
	if err != nil {
		// 产物缺失是**部署事故**，不能静默回一个空 200。
		s.writeError(w, internalError(err.Error()))
		return
	}
	s.writeHTML(w, index)
}

// assetMIME 是内嵌产物里用到的扩展名 → MIME。
//
// 为什么不完全依赖 mime.TypeByExtension：`.map`（sourcemap）在多数系统的
// mime.types 里没有注册，会退化成 application/octet-stream —— 浏览器与
// 调试器都不认，sourcemap 就等于没交。因此对**我们自己产物**涉及的扩展名
// 显式声明，其它仍交给标准库。
var assetMIME = map[string]string{
	".js":   "text/javascript; charset=utf-8",
	".mjs":  "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".map":  "application/json; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".svg":  "image/svg+xml",
}

// contentTypeFor 返回文件对应的 MIME（显式表优先，其次标准库）。
func contentTypeFor(name string) string {
	if ct, ok := assetMIME[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// handleAsset 返回内嵌静态资源（GET /assets/{name}）。
//
// 只从 embed FS 里取，不碰磁盘：这样二进制可单文件分发，也不会有
// 路径穿越（web.Asset 已拒绝非法路径与反斜杠）。
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	body, ok := web.Asset(name)
	if !ok {
		s.writeError(w, notFound("资源 /assets/"+name+" 不存在"))
		return
	}
	ct := contentTypeFor(name)
	w.Header().Set("Content-Type", ct)
	// 产物文件名不带哈希（重建后内容会变），因此统一 no-cache：
	// 让它每次都回源校验，避免"改了前端但浏览器还是旧画面"。
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeHTML 输出 HTML 页面（与 writeJSON 对称，便于统一加头部）。
func (s *Server) writeHTML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// apiError 是带 HTTP 语义的错误。
type apiError struct {
	status  int
	code    string
	message string
	field   string
}

func (e *apiError) Error() string { return e.message }

func badRequest(field, format string, args ...any) *apiError {
	return &apiError{
		status: http.StatusBadRequest, code: "bad_request",
		message: fmt.Sprintf(format, args...), field: field,
	}
}

func notFound(message string) *apiError {
	return &apiError{status: http.StatusNotFound, code: "not_found", message: message}
}

func internalError(message string) *apiError {
	return &apiError{status: http.StatusInternalServerError, code: "internal_error", message: message}
}

// writeError 把错误渲染成 JSON。非 apiError 一律 500，但**仍给出可读信息**。
func (s *Server) writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.status, ErrorResponse{Error: ae.code, Message: ae.message, Field: ae.field})
		return
	}
	writeJSON(w, http.StatusInternalServerError, ErrorResponse{
		Error: "internal_error", Message: err.Error(),
	})
}

// writeJSON 序列化并写出。
//
// 用 json.Marshal 而不是 Encoder：Encoder 会按 map 迭代序输出对象键，
// 而我们要保证"同一输入 → 逐字节一致的响应"（坐标确定性的回归用例正是深比较字节）。
// json.Marshal 对结构体按字段顺序输出，因此结果是稳定的。
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal_error","message":"响应序列化失败"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// entityIDFromPath 从 /api/entity/{id} 取出 id。
//
// 用 PathValue 语义手写（不引第三方路由）：Go 1.22 的 ServeMux 已支持模式匹配，
// 但这里保持与 Routes() 声明一致的最简实现，避免两处模式语法不一致。
func entityIDFromPath(path string) (string, error) {
	const prefix = "/api/entity/"
	if !strings.HasPrefix(path, prefix) {
		return "", notFound("路径 " + path + " 不存在")
	}
	id := strings.TrimPrefix(path, prefix)
	id = strings.Trim(id, "/")
	if id == "" {
		return "", badRequest("id", "缺少实体 ID：用法 /api/entity/{id}")
	}
	if strings.Contains(id, "/") {
		return "", badRequest("id", "实体 ID 不能包含 '/'，收到 %q", id)
	}
	return id, nil
}
