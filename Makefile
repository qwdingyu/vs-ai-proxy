# VS AI Proxy 跨平台构建脚本
# 用法:
#   make build         构建当前平台
#   make build-all     构建所有平台
#   make install       构建所有平台便携包（可执行文件）
#   make release       构建并打包所有平台（压缩包）
#   make release-notes 生成当前 tag 的 GitHub Release 正文
#   make tool-check    工具调用专项核查
#   make release-check 发布前完整核查
#   make clean         清理构建产物

APP_NAME     := vs-ai-proxy
MAIN_PATH    := ./cmd/server
OUTPUT_DIR   := ./dist
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION_TAG  := $(patsubst v%,%,$(VERSION))

# 构建参数
LDFLAGS := -s -w -X main.version=$(VERSION)
GO_WINRES := $(shell command -v go-winres 2>/dev/null || echo "$$(go env GOPATH)/bin/go-winres")
WINDOWS_RSRC := cmd/server/rsrc_windows_amd64.syso

# 所有目标平台（GOOS/GOARCH）
PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64 \
	windows/amd64

# 用户-facing 名称映射：${GOOS}/${GOARCH} -> 文件名片段
PLATFORM_ALIAS := \
	darwin/amd64:macos-x64 \
	darwin/arm64:macos-arm64 \
	linux/amd64:linux-x64 \
	linux/arm64:linux-arm64 \
	windows/amd64:windows-x64

# ─── 默认目标 ──────────────────────────────────────────
.PHONY: all build build-all install release release-notes tool-check release-check windows-res ensure-windows-res clean

all: build

# ─── 构建当前平台 ──────────────────────────────────────
build:
	@echo "🔨 构建 $(APP_NAME) $(VERSION) 当前平台..."
	go build -ldflags="$(LDFLAGS)" -o $(APP_NAME)$(suffix $(shell go env GOOS))$(if $(filter windows,$(shell go env GOOS)),.exe,) $(MAIN_PATH)
	@echo "✅ 构建完成: $(APP_NAME)$(if $(filter windows,$(shell go env GOOS)),.exe,)"

# ─── 生成 Windows exe 图标资源 ─────────────────────────
windows-res:
	@python3 tools/generate_windows_icon.py
	@if [ ! -x "$(GO_WINRES)" ]; then \
		echo "go-winres not found. Install with: go install github.com/tc-hib/go-winres@v0.3.3"; \
		exit 1; \
	fi
	@"$(GO_WINRES)" make --in winres/winres.json --arch amd64 --out cmd/server/rsrc

ensure-windows-res:
	@test -f "$(WINDOWS_RSRC)" || { echo "missing $(WINDOWS_RSRC); run make windows-res"; exit 1; }

# ─── 构建所有平台 ──────────────────────────────────────
build-all: ensure-windows-res
	@echo "🔨 构建 $(APP_NAME) $(VERSION) 所有平台..."
	@mkdir -p $(OUTPUT_DIR)
	@for plat in $(PLATFORMS); do \
		GOOS=$$(echo $$plat | cut -d/ -f1); \
		GOARCH=$$(echo $$plat | cut -d/ -f2); \
		EXT=$$( [ "$$GOOS" = "windows" ] && echo ".exe" || echo "" ); \
		ALIAS=$$(echo $$plat | sed -e 's|darwin/amd64|macos-x64|' -e 's|darwin/arm64|macos-arm64|' -e 's|linux/amd64|linux-x64|' -e 's|linux/arm64|linux-arm64|' -e 's|windows/amd64|windows-x64|'); \
		NAME="$(APP_NAME)-v$(VERSION_TAG)-$${ALIAS}$${EXT}"; \
		echo "  → $$GOOS/$$GOARCH ..."; \
		GOOS=$$GOOS GOARCH=$$GOARCH go build -ldflags="$(LDFLAGS)" -o "$(OUTPUT_DIR)/$$NAME" $(MAIN_PATH); \
	done
	@echo "✅ 全部构建完成: $(OUTPUT_DIR)/"
	@ls -lh $(OUTPUT_DIR)

# ─── 构建所有平台便携包（仅可执行文件，最直观） ──────────
install: build-all
	@echo "📦 生成便携二进制包..."
	@for plat in $(PLATFORMS); do \
		GOOS=$$(echo $$plat | cut -d/ -f1); \
		GOARCH=$$(echo $$plat | cut -d/ -f2); \
		EXT=$$( [ "$$GOOS" = "windows" ] && echo ".exe" || echo "" ); \
		ALIAS=$$(echo $$plat | sed -e 's|darwin/amd64|macos-x64|' -e 's|darwin/arm64|macos-arm64|' -e 's|linux/amd64|linux-x64|' -e 's|linux/arm64|linux-arm64|' -e 's|windows/amd64|windows-x64|'); \
		SRC="$(OUTPUT_DIR)/$(APP_NAME)-v$(VERSION_TAG)-$${ALIAS}$${EXT}"; \
		DST="$(OUTPUT_DIR)/$(APP_NAME)-$${ALIAS}$${EXT}"; \
		mv "$$SRC" "$$DST"; \
	done
	@echo "✅ 便携包已生成: $(OUTPUT_DIR)/"
	@ls -lh $(OUTPUT_DIR)

# ─── 构建并打包 ────────────────────────────────────────
release:
	@echo "📦 打包压缩包..."
	@bash .bin/release-all.sh
	@echo "✅ 发布包已生成: $(OUTPUT_DIR)/"
	@ls -lh $(OUTPUT_DIR)

# ─── 生成规范 Release 说明 ─────────────────────────────
release-notes:
	@bash .bin/release-notes.sh

# ─── 工具调用专项核查 ──────────────────────────────────
tool-check:
	@bash tests/tool_call_release_check.sh

# ─── 发布前完整核查 ────────────────────────────────────
release-check: tool-check
	go test ./... -count=1
	go test -race ./cmd/server ./internal/proxy ./internal/provider ./internal/config ./internal/update ./internal/store ./internal/api ./internal/requestmeta ./web -count=1
	go vet ./...
	@tmp_js=$$(mktemp /tmp/vs-ai-proxy-script.XXXXXX.js); \
		sed -n '/<script>/,/<\/script>/p' web/dist/index.html | sed '1d;$$d' > "$$tmp_js"; \
		node --check "$$tmp_js"; \
		rm -f "$$tmp_js"
	@git diff --check
	@rm -rf .bin/windows-build-check
	@mkdir -p .bin/windows-build-check
	GOOS=windows GOARCH=amd64 go build -ldflags='-s -w -X main.version=$(VERSION)-release-check' -o .bin/windows-build-check/vs-ai-proxy.exe $(MAIN_PATH)
	@rm -rf .bin/windows-build-check .bin/logs.json
	@echo "RELEASE_CHECK_OK"

# ─── 清理 ──────────────────────────────────────────────
clean:
	rm -rf $(OUTPUT_DIR)
	rm -f $(APP_NAME)
	rm -f $(APP_NAME).exe
	@echo "🧹 清理完成"
