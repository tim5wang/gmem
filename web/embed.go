// Package web 把 Graph View 的前端产物编进二进制（docs/TD.md §9「内嵌前端」）。
//
// # 边界（本包的**结构性不变量**）
//
//	web/src/   构建输入（npm / node_modules / esbuild 源码），**永不 embed**
//	web/dist/  构建产物，**提交进仓库**，本包只 embed 它
//
// 这条边界的意义：`go install` / `go build` 不需要 Node、不需要 npm、不需要网络。
// 代价是 dist 必须提交，且改前端后要重建（由 `make web-check` 兜住不同步）。
//
// 守卫在 web/embed_test.go：扫描全仓 Go 源码，禁止出现 web/src 路径；
// 并断言 embed 出来的内容里没有 src 残留、index.html 与 assets 都在。
//
// 本包只依赖标准库（embed / io / io/fs），不做任何 I/O、不 import 任何后端，
// 因此可以被 server 直接依赖而不破坏"接口层不碰 SQL"的分层（D6）。
package web

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
)

// distFS 只 embed 产物目录。
//
// 注意：这里**只能**写 dist —— 写成 `//go:embed dist ../web/src` 之类会把
// node_modules（数万文件）塞进二进制，且让"运行时零依赖"的承诺变得可疑。
//
//go:embed dist
var distFS embed.FS

// assets 是 dist 的子树视图（去掉了 "dist/" 前缀）。
var assets = mustSub(distFS, "dist")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		// embed 内容在编译期固定：拿不到子树说明 embed 指令写错了，
		// 这时启动即失败比运行到 GET / 时才 500 更容易发现。
		panic(fmt.Sprintf("web: embed 子树 %q 不存在: %v", dir, err))
	}
	return sub
}

// FS 返回只读的产物文件系统（根目录就是 dist/，含 index.html 与 assets/）。
func FS() fs.FS { return assets }

// IndexHTML 返回 index.html 的全部字节。
//
// 直接给出字节而不是让调用方自己 Open：GET / 是最热的路径，且必须
// 以 text/html 返回；把"读哪份文件"固化在这里，避免路由层猜路径。
func IndexHTML() ([]byte, error) {
	f, err := assets.Open("index.html")
	if err != nil {
		return nil, fmt.Errorf("web: 缺少 index.html（web/dist 未提交或未构建？）: %w", err)
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Asset 返回 assets/ 下某个文件的字节与是否存在。
//
// name 是**相对 assets/ 的路径**（如 "app.js"），不接受 ".." 或绝对路径：
// 路径穿越在 embed FS 上本来也走不通，但显式拒绝能让错误更早更清楚。
func Asset(name string) ([]byte, bool) {
	if name == "" {
		return nil, false
	}
	for _, r := range name {
		if r == '\\' {
			return nil, false
		}
	}
	if fs.ValidPath(name) == false || name == "." {
		return nil, false
	}
	f, err := assets.Open("assets/" + name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, false
	}
	return buf, true
}
