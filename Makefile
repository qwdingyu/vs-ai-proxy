# VS AI Proxy 跨平台构建脚本
# 用法:
#   make build         构建当前平台
#   make build-all     构建所有平台
#   make install       构建所有平台便携包（可执行文件）
#   make release       构建并打包所有平台（压缩包）
#   make release-notes 生成当前 tag 的 GitHub Release 正文
#   make clean         清理构建产物

APP_NAME     := vs-ai-proxy
MAIN_PATH    := ./cmd/server
OUTPUT_DIR   := ./dist
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION_TAG  := $(patsubst v%,%,$(VERSION))

# 构建参数
LDFLAGS := -s -w -X main.version=$(VERSION)

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
.PHONY: all build build-all install release release-notes clean

all: build

# ─── 构建当前平台 ──────────────────────────────────────
build:
	@echo "🔨 构建 $(APP_NAME) v$(VERSION) 当前平台..."
	go build -ldflags="$(LDFLAGS)" -o $(APP_NAME)$(suffix $(shell go env GOOS))$(if $(filter windows,$(shell go env GOOS)),.exe,) $(MAIN_PATH)
	@echo "✅ 构建完成: $(APP_NAME)$(if $(filter windows,$(shell go env GOOS)),.exe,)"

# ─── 构建所有平台 ──────────────────────────────────────
build-all:
	@echo "🔨 构建 $(APP_NAME) v$(VERSION) 所有平台..."
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

# ─── 清理 ──────────────────────────────────────────────
clean:
	rm -rf $(OUTPUT_DIR)
	rm -f $(APP_NAME)
	rm -f $(APP_NAME).exe
	@echo "🧹 清理完成"
