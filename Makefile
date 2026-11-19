# Gmem 构建入口（docs/TD.md §9「工程约束」/ §13 风险清单）
#
# 三个目标的关系：
#   web-build  用 Node 重建 web/dist 产物（只在改前端时需要 Node）
#   web-check  在**同层深**的临时目录重建一次，与仓库内 web/dist 逐字节比对；
#              不一致就非零退出并列出具体文件 —— 钉死"改了源码忘了重建"
#   test       常规 Go 门禁（build / vet / gofmt / test）
#
# 关键要求：未装 Node 时 `go build ./...` 必须仍成功（dist 已提交），
# 因此 web-check 在缺 Node 时**跳过并提示**，绝不让本地/CI 因为前端而失败。

GO      ?= go
NODE    ?= node
NPM     ?= npm
DIST     := web/dist
SRC      := web/src
# 校验用临时产物目录：必须是 DIST 的**同层兄弟**，否则 esbuild 算出的
# sourcemap 相对路径（../../src/js/...）会变，比对必然假红。
DIST_DEV := web/dist-check

.PHONY: all build test web-build web-check web-test clean

all: build

build:
	$(GO) build ./...

test:
	$(GO) build ./...
	$(GO) vet ./...
	@out="$$($(GO) fmt ./... 2>&1)"; if [ -n "$$out" ]; then echo "$$out"; echo "gofmt 需要修正（见上）"; exit 1; fi
	$(GO) test ./... -count=1

# ---- 前端 -----------------------------------------------------------------

# web-build 用 Node 重建产物。未装 Node 时**失败**是正确行为（你要求重建但工具不在），
# 但它不影响 go build：仓库内 web/dist 已提交。
web-build:
	@if ! command -v $(NODE) >/dev/null 2>&1 || ! command -v $(NPM) >/dev/null 2>&1; then \
		echo "web-build: 未检测到 node/npm，无法重建前端。"; \
		echo "web-build: 仓库内 $(DIST) 已提交，go build 不受影响。"; \
		exit 1; \
	fi
	cd $(SRC) && $(NPM) install --no-audit --no-fund --silent
	cd $(SRC) && $(NPM) run build

web-test:
	@if ! command -v $(NODE) >/dev/null 2>&1 || ! command -v $(NPM) >/dev/null 2>&1; then \
		echo "web-test: 未检测到 node/npm，跳过前端单测。"; \
		exit 0; \
	fi
	cd $(SRC) && $(NPM) test

# web-check 必须在**同一个 shell 里**完成"判定 → 可能提前退出 → 重建 → 比对"。
# 为什么不能分行写：make 的每行 recipe 都是独立 shell，上一行的 exit 0 拦不住
# 下一行照样执行（曾因此出现过"提示已跳过，却还是去调 node"的假红）。
web-check:
	@set -e; \
	if ! command -v $(NODE) >/dev/null 2>&1 || ! command -v $(NPM) >/dev/null 2>&1; then \
		echo "web-check: 未检测到 node/npm —— 跳过产物同步校验（不算失败）。"; \
		echo "web-check: 已提交的 $(DIST) 仍可用于 go build；要改前端请安装 Node 后重跑。"; \
		exit 0; \
	fi; \
	if [ ! -d $(SRC)/node_modules ]; then \
		echo "web-check: $(SRC)/node_modules 缺失，先安装依赖…"; \
		cd $(SRC) && $(NPM) install --no-audit --no-fund --silent; \
		cd ..; \
	fi; \
	if [ ! -d $(DIST) ]; then \
		echo "web-check: $(DIST) 不存在 —— 产物必须提交进仓库（否则 go build 会失败）。"; \
		exit 1; \
	fi; \
	rm -rf $(DIST_DEV); \
	cd $(SRC) && $(NODE) build.mjs --out ../dist-check >/dev/null; \
	cd $(CURDIR); \
	echo "web-check: 重建完成，开始与仓库内 $(DIST) 逐文件比对…"; \
	if out=$$(diff -r -q $(DIST) $(DIST_DEV) 2>&1); then \
		echo "web-check: $(DIST) 与 $(SRC) 同步（逐字节一致）。"; \
		rm -rf $(DIST_DEV); \
	else \
		echo "$$out" | sed 's/^/    /'; \
		echo "web-check: $(DIST) 与 $(SRC) **不同步** —— 改了前端但没重建产物（具体文件见上）。"; \
		echo "web-check: 修复方式：make web-build 后提交 $(DIST)。"; \
		rm -rf $(DIST_DEV); \
		exit 1; \
	fi

clean:
	rm -rf $(DIST_DEV)
	$(GO) clean -testcache
