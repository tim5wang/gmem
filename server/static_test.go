package server_test

import (
	"net/http"
	"strings"
	"testing"
)

// ===========================================================================
// 内嵌前端的路由（P4-b）
//
// 本卡把 GET / 的返回从"服务说明 JSON"换成了内嵌的 index.html。
// 这些用例钉死新的契约，并且**保留**原有断言（状态码 200、JSON 接口仍可用），
// 不是把旧用例删掉换成新用例。
// ===========================================================================

// TestRootReturnsEmbeddedIndex GET / 返回 HTML 页面（含前端入口）。
func TestRootReturnsEmbeddedIndex(t *testing.T) {
	h := newHarness(t)
	status, body := h.get("/")
	if status != http.StatusOK {
		t.Fatalf("GET / status=%d, want 200 (body=%s)", status, body)
	}
	html := string(body)
	if !strings.Contains(html, "<html") || !strings.Contains(html, "</html>") {
		t.Fatalf("GET / 未返回 HTML 文档：%s", truncate(html, 300))
	}
	// 页面必须自己带上渲染入口：否则"返回了 HTML"也可能是张空页。
	for _, want := range []string{"assets/app.js", "assets/styles.css", "graph-svg"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}
	if strings.Contains(html, `"service":"gmem"`) {
		t.Error("GET / 仍在返回旧的服务说明 JSON（本卡要求改为返回 index.html）")
	}
}

// TestRootContentTypeIsHTML 必须是 text/html，否则浏览器会当纯文本显示源码。
func TestRootContentTypeIsHTML(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.http.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / Content-Type=%q, want text/html", ct)
	}
}

// TestAssetsServed 内嵌静态资源可访问，且 MIME 正确。
func TestAssetsServed(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		path string
		want string // Content-Type 前缀
	}{
		{"/assets/app.js", "text/javascript"},
		{"/assets/styles.css", "text/css"},
		{"/assets/app.js.map", "application/json"},
	}
	for _, c := range cases {
		status, body := h.get(c.path)
		if status != http.StatusOK {
			t.Errorf("GET %s status=%d, want 200", c.path, status)
			continue
		}
		if len(body) == 0 {
			t.Errorf("GET %s 返回空内容", c.path)
		}
		resp, err := http.Get(h.http.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		ct := resp.Header.Get("Content-Type")
		resp.Body.Close()
		if !strings.HasPrefix(ct, c.want) {
			t.Errorf("GET %s Content-Type=%q, want 前缀 %q", c.path, ct, c.want)
		}
	}
}

// TestAssetsUnknown404 不存在的资源必须 404（不得静默回 index.html）。
func TestAssetsUnknown404(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/assets/nope.js", "/assets/", "/assets/../server.go"} {
		if status, _ := h.get(p); status != http.StatusNotFound {
			t.Errorf("GET %s status=%d, want 404", p, status)
		}
	}
}

// TestRootUnknownPath404 API 面的未知路径仍回 JSON 404。
//
// P6-1 把页面路由改成了通配（六页共用一份 index.html），因此原来那条
// "GET /graph 必须 404" 的断言被**换成新契约**：页面路径回 HTML，
// 而 **API 面**（/api/*）与**带扩展名的文件**仍必顶 404。
// 旧断言的意图（"URL 打错不得被伪装成能开"）在这里逐条保留。
func TestRootUnknownPath404(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/api/nope", "/api", "/server.go", "/assets-nope.png"} {
		status, body := h.get(p)
		if status != http.StatusNotFound {
			t.Fatalf("GET %s status=%d, want 404 (body=%s)", p, status, truncate(string(body), 200))
		}
		if !strings.Contains(string(body), `"error"`) {
			t.Errorf("GET %s 的 404 未返回 JSON 错误体：%s", p, truncate(string(body), 200))
		}
	}
}

// TestPageRoutesReturnIndex 页面路由通配：/、/recall 与任意页面路径回同一份 HTML。
//
// 这是六页的基石（前置 1）：刷新 /recall?q=acme 必须能拿到前端入口，
// 而不是 404。
func TestPageRoutesReturnIndex(t *testing.T) {
	h := newHarness(t)
	_, rootBody := h.get("/")
	for _, p := range []string{"/recall", "/recall?q=acme", "/review", "/metrics", "/nope-page"} {
		status, body := h.get(p)
		if status != http.StatusOK {
			t.Errorf("GET %s status=%d, want 200 (body=%s)", p, status, truncate(string(body), 200))
			continue
		}
		// 必须是**同一份** index.html：页面选择由前端客户端路由决定。
		if string(body) != string(rootBody) {
			t.Errorf("GET %s 返回的 HTML 与 GET / 不一致（页面路由必须是同一份产物）", p)
		}
		if !strings.Contains(string(body), "assets/app.js") {
			t.Errorf("GET %s 未返回前端入口：%s", p, truncate(string(body), 200))
		}
	}
}

// TestPageRouteContentTypeIsHTML 页面路由必须 text/html（否则浏览器当纯文本显示源码）。
func TestPageRouteContentTypeIsHTML(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.http.URL + "/recall?q=acme")
	if err != nil {
		t.Fatalf("GET /recall: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /recall status=%d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /recall Content-Type=%q, want text/html", ct)
	}
}

// TestRootRejectsNonGet 非 GET 方法 405（页面路由与静态资源都不接受写方法）。
func TestRootRejectsNonGet(t *testing.T) {
	h := newHarness(t)
	if status, _ := h.postJSON("/", `{}`); status != http.StatusMethodNotAllowed {
		t.Errorf("POST / status=%d, want 405", status)
	}
	if status, _ := h.postJSON("/recall", `{}`); status != http.StatusMethodNotAllowed {
		t.Errorf("POST /recall status=%d, want 405", status)
	}
	if status, _ := h.postJSON("/assets/app.js", `{}`); status != http.StatusMethodNotAllowed {
		t.Errorf("POST /assets/app.js status=%d, want 405", status)
	}
}

// TestStaticRoutesAreReadOnly 静态路由同样必须只读（只读优先，TD §9）。
func TestStaticRoutesAreReadOnly(t *testing.T) {
	h := newHarness(t)
	routes := h.srv.StaticRoutes()
	if len(routes) == 0 {
		t.Fatal("StaticRoutes() 为空（守卫失效）")
	}
	for _, r := range routes {
		if r.Method != http.MethodGet {
			t.Errorf("静态路由出现了非 GET 方法：%s %s", r.Method, r.Pattern)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
