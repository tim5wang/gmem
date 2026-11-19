package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ===========================================================================
// 内嵌前端（P4-b）的**真二进制**端到端验证
//
// 与 server/static_test.go 的分工：那边用 httptest 直接挂 Handler（快、覆盖
// 每个分支），这边起**真的 gmem 进程**再连真的 TCP —— 只有这样才能证明
// "//go:embed 的产物在 go build 出来的二进制里"这件事，而不是"在测试进程里"。
//
// 后台进程在本环境不可用（shell 侧禁止），因此复用本包既有的
// TestServeSmoke 手法：测试二进制自己作为子进程，在子进程里跑真实 run()。
// ===========================================================================

// startServe 起一个真实 serve 子进程并等到 /api/graph 可用，返回 base URL。
func startServe(t *testing.T, dbPath string) (base string, out *strings.Builder) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	out = &strings.Builder{}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1",
		"GMEM_MAIN_ARGS="+strings.Join([]string{"serve", "--db", dbPath, "--addr", addr}, argsSep))
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	base = "http://" + addr
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 100; i++ {
		resp, err := client.Get(base + "/api/graph")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, out
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("服务未在 5s 内就绪\n子进程输出：%s", out.String())
	return "", out
}

// TestServeServesEmbeddedFrontend 是 P4-b 的接线证明：
// 真二进制能返回页面、能返回 JS/CSS/sourcemap、非法 budget 仍 400。
func TestServeServesEmbeddedFrontend(t *testing.T) {
	base, out := startServe(t, filepath.Join(t.TempDir(), "gmem.db"))
	client := &http.Client{Timeout: 5 * time.Second}

	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 0, 64*1024)
		chunk := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(chunk)
			buf = append(buf, chunk[:n]...)
			if err != nil {
				break
			}
		}
		return resp, buf
	}

	// 1) GET / → HTML 页面（不再是 JSON 服务说明）。
	resp, body := get("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status=%d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / Content-Type=%q, want text/html", ct)
	}
	html := string(body)
	for _, want := range []string{"<html", "assets/app.js", "graph-svg"} {
		if !strings.Contains(html, want) {
			t.Errorf("返回的 index.html 缺少 %q", want)
		}
	}

	// 2) 静态资源：JS / CSS / sourcemap 都能取到，且 MIME 可用。
	for _, c := range []struct{ path, wantType, wantBody string }{
		{"/assets/app.js", "text/javascript", "d3-force"},
		{"/assets/styles.css", "text/css", ".stage"},
		{"/assets/app.js.map", "application/json", "\"version\": 3"},
	} {
		resp, buf := get(c.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status=%d, want 200", c.path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.wantType) {
			t.Errorf("GET %s Content-Type=%q, want 前缀 %q", c.path, ct, c.wantType)
		}
		if !strings.Contains(string(buf), c.wantBody) {
			t.Errorf("GET %s 内容不像预期（缺 %q，长度 %d）", c.path, c.wantBody, len(buf))
		}
	}

	// 3) 不存在的资源必须 404（不得静默回 index.html）。
	if resp, _ := get("/assets/nope.js"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /assets/nope.js status=%d, want 404", resp.StatusCode)
	}

	// 4) JSON 接口未被前端路由顶掉：overview 仍能取到图。
	respGraph, bodyGraph := get("/api/graph?budget=2")
	if respGraph.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/graph status=%d, want 200 (body=%s)", respGraph.StatusCode, bodyGraph)
	}
	if !strings.Contains(string(bodyGraph), `"meta"`) {
		t.Errorf("GET /api/graph 返回体不像 GraphResponse：%s", string(bodyGraph[0:minInt(len(bodyGraph), 200)]))
	}

	// 5) budget 超硬上限 → 400 且信息可读（前端界面上要显式报出来）。
	respBad, bodyBad := get("/api/graph?budget=5000")
	if respBad.StatusCode != http.StatusBadRequest {
		t.Errorf("budget=5000 status=%d, want 400", respBad.StatusCode)
	}
	if !strings.Contains(string(bodyBad), "2000") {
		t.Errorf("400 的错误信息应点明硬上限 2000：%s", string(bodyBad))
	}

	// 6) 启动横幅仍写明监听地址（原断言不放宽）。
	if !strings.Contains(out.String(), "gmem serve 已启动") {
		t.Errorf("启动输出缺少横幅：%s", out.String())
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
