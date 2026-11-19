package main

import (
	"fmt"
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
// 入口测试（cmd/gmem）
//
// 做法：测试进程用**自己**作为子进程（TestMain 里的 GMEM_MAIN_CHILD 分支），
// 在子进程里调用真实的 run()。这样验的是**真的二进制入口**——包括退出码、
// stdin/stdout 与"能不能真的连上"——而不是一份只在测试里存在的近似调用。
//
// 为什么不直接在同一个进程里调 runServe：serve 会一直阻塞（那是它的正确行为），
// 而退出码正是本卡的验收项之一，只能靠跨进程观察。
// ===========================================================================

// childEnv 让测试二进制变成"真实的 gmem 入口"。
const childEnv = "GMEM_MAIN_CHILD"

// argsSep 用来把多参数塞进一个环境变量（不依赖 shell 转义）。
// 不能用 NUL：exec 明确拒绝含 NUL 的环境变量。这里用 US（unit separator）。
const argsSep = "\x1f"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		var argv []string
		if v := os.Getenv("GMEM_MAIN_ARGS"); v != "" {
			argv = strings.Split(v, argsSep)
		}
		if err := run(argv, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "gmem: "+err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runChild 在子进程里跑一次 gmem，返回退出码与合并输出。
func runChild(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1", "GMEM_MAIN_ARGS="+strings.Join(args, argsSep))
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("子进程启动失败: %v", err)
		}
	}
	return code, stdout.String(), stderr.String()
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// TestDefaultAddrIsLoopback 默认地址**必须**是回环（不得 0.0.0.0）。
//
// 这不是风格问题：默认绑 0.0.0.0 等于把整个记忆库（含"谁在哪工作"）广播到局域网。
func TestDefaultAddrIsLoopback(t *testing.T) {
	host, port, err := net.SplitHostPort(defaultAddr)
	if err != nil {
		t.Fatalf("defaultAddr %q 不是合法的 host:port: %v", defaultAddr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("defaultAddr 的 host %q 不是 IP（无法判定是否回环）", host)
	}
	if !ip.IsLoopback() {
		t.Fatalf("defaultAddr=%q 必须绑回环（127.0.0.1），不得对外暴露", defaultAddr)
	}
	if port != "7700" {
		t.Errorf("默认端口应为 7700，实际 %q", port)
	}
	if strings.Contains(defaultAddr, "0.0.0.0") {
		t.Fatalf("defaultAddr 不得含 0.0.0.0：%q", defaultAddr)
	}
}

// TestNoSubcommand 无子命令 → 非零退出 + 用法提示。
func TestNoSubcommand(t *testing.T) {
	code, _, stderr := runChild(t)
	if code == 0 {
		t.Fatal("缺少子命令必须非零退出")
	}
	if !strings.Contains(stderr, "serve") {
		t.Errorf("错误信息应提示可用的子命令：%q", stderr)
	}
}

// TestUnknownSubcommand 未知子命令 → 非零退出 + 用法提示（不假装支持不存在的命令）。
//
// P5 之前这条用例拿 "recall" 当未知子命令的例子（那时只提供 serve）。
// P5 把五动词做成了真实子命令，"recall 必须是未知命令"这个前提已被取代；
// 用例**要证明的东西没变**：给一个不认识的名字必须非零退出并提示可用命令。
// 因此这里只把例子换成一个真正不存在的名字，判定条件不动。
func TestUnknownSubcommand(t *testing.T) {
	code, _, stderr := runChild(t, "nosuchverb", "--q", "x")
	if code == 0 {
		t.Fatal("未知子命令必须非零退出")
	}
	if !strings.Contains(stderr, "serve") {
		t.Errorf("错误信息应提示可用的子命令（含 serve）：%q", stderr)
	}
}

// TestServeRequiresDB 缺 --db → 非零退出 + 可读提示（不得起一个空库服务）。
func TestServeRequiresDB(t *testing.T) {
	code, _, stderr := runChild(t, "serve")
	if code == 0 {
		t.Fatal("缺 --db 必须非零退出")
	}
	if !strings.Contains(stderr, "--db") {
		t.Errorf("错误信息必须点明 --db：%q", stderr)
	}
}

// TestServeUnopenableDB 库打不开 → 非零退出 + 可读错误（不是裸 panic）。
func TestServeUnopenableDB(t *testing.T) {
	// 父目录不存在 → SQLite 无法打开（真实失败路径，不是模拟）。
	bad := filepath.Join(t.TempDir(), "no-such-dir", "gmem.db")
	code, _, stderr := runChild(t, "serve", "--db", bad, "--addr", "127.0.0.1:0")
	if code == 0 {
		t.Fatal("库打不开必须非零退出")
	}
	if !strings.Contains(stderr, "数据库") {
		t.Errorf("错误信息必须说明是数据库打不开：%q", stderr)
	}
	if strings.Contains(stderr, "panic:") || strings.Contains(stderr, "goroutine ") {
		t.Errorf("不得以 panic 收场：%q", stderr)
	}
}

// TestServePortInUse 端口占用 → 非零退出 + 可读错误（含"端口"或"监听"字样与建议）。
func TestServePortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	dbPath := filepath.Join(t.TempDir(), "gmem.db")
	code, _, stderr := runChild(t, "serve", "--db", dbPath, "--addr", addr)
	if code == 0 {
		t.Fatal("端口占用必须非零退出")
	}
	if !strings.Contains(stderr, "监听") && !strings.Contains(stderr, "端口") {
		t.Errorf("错误信息必须说明监听失败/端口占用：%q", stderr)
	}
}

// TestServeSmoke 真起一个服务：能连上、/api/graph 返回 200、默认只绑回环。
func TestServeSmoke(t *testing.T) {
	// 先占一个空闲端口再释放（serve 需要具体端口以便父进程访问）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dbPath := filepath.Join(t.TempDir(), "gmem.db")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1",
		"GMEM_MAIN_ARGS="+strings.Join([]string{"serve", "--db", dbPath, "--addr", addr}, argsSep))
	// syncBuffer（而不是 strings.Builder）：exec.Cmd 在独立 goroutine 里向它写入，
	// 而本测试同时读 out.String()。strings.Builder 不支持并发读写，-race 会报
	// DATA RACE（这是 P4 遗留的**测试**缺陷，与产品代码无关）。
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	base := "http://" + addr
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	ok := false
	for i := 0; i < 100; i++ {
		resp, err := client.Get(base + "/api/graph")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ok = true
				break
			}
			lastErr = fmt.Errorf("status=%d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("服务未在 5s 内就绪：%v\n子进程输出：%s", lastErr, out.String())
	}

	// 根路径给出可读说明（而不是 404 空白）。
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / status=%d, want 200", resp.StatusCode)
	}

	// 启动横幅必须写明绑的是回环地址（便于使用者确认没有对外暴露）。
	if !strings.Contains(out.String(), addr) {
		t.Errorf("启动输出未回显监听地址 %q：%s", addr, out.String())
	}
	if strings.Contains(out.String(), "0.0.0.0") {
		t.Errorf("启动输出出现了 0.0.0.0（不得对外暴露）：%s", out.String())
	}
}
