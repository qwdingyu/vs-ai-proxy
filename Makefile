# VS AI Proxy 跨平台构建脚本
# 用法:
#   make build         构建当前平台
#   make build-all     构建所有平台
#   make release       构建并打包所有平台（压缩包）
#   make clean         清理构建产物

APP_NAME     := vs-ai-proxy
MAIN_PATH    := ./cmd/server
OUTPUT_DIR   := ./dist
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION_TAG  := $(patsubst v%,%,$(VERSION))

# 构建参数
LDFLAGS := -s -w -X main.version=$(VERSION)

# 所有目标平台
PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64 \
	windows/amd64

# ─── 默认目标 ──────────────────────────────────────────
.PHONY: all build build-all release clean

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
		NAME="$(APP_NAME)-v$(VERSION_TAG)-$${GOOS}-$${GOARCH}$${EXT}"; \
		echo "  → $$GOOS/$$GOARCH ..."; \
		GOOS=$$GOOS GOARCH=$$GOARCH go build -ldflags="$(LDFLAGS)" -o "$(OUTPUT_DIR)/$$NAME" $(MAIN_PATH); \
	done
	@echo "✅ 全部构建完成: $(OUTPUT_DIR)/"
	@ls -lh $(OUTPUT_DIR)

# ─── 构建并打包 ────────────────────────────────────────
release: build-all
	@echo "📦 打包压缩包..."
	@for plat in $(PLATFORMS); do \
		GOOS=$$(echo $$plat | cut -d/ -f1); \
		GOARCH=$$(echo $$plat | cut -d/ -f2); \
		EXT=$$( [ "$$GOOS" = "windows" ] && echo ".exe" || echo "" ); \
		DIR="$(APP_NAME)-v$(VERSION_TAG)-$${GOOS}-$${GOARCH}"; \
		BIN="$(APP_NAME)-v$(VERSION_TAG)-$${GOOS}-$${GOARCH}$${EXT}"; \
		mkdir -p "$(OUTPUT_DIR)/$$DIR"; \
		cp "$(OUTPUT_DIR)/$$BIN" "$(OUTPUT_DIR)/$$DIR/$(APP_NAME)$${EXT}"; \
		cp README.md LICENSE "$(OUTPUT_DIR)/$$DIR/" 2>/dev/null || true; \
		cd $(OUTPUT_DIR); \
		if [ "$$GOOS" = "windows" ]; then \
			zip -r "$$DIR.zip" "$$DIR" > /dev/null 2>&1; \
			echo "  📦 $$DIR.zip"; \
		else \
			tar czf "$$DIR.tar.gz" "$$DIR" 2>/dev/null; \
			echo "  📦 $$DIR.tar.gz"; \
		fi; \
		rm -rf "$$DIR"; \
		cd ..; \
	done
	@echo "✅ 发布包已生成: $(OUTPUT_DIR)/"
	@ls -lh $(OUTPUT_DIR)

# ─── 清理 ──────────────────────────────────────────────
clean:
	rm -rf $(OUTPUT_DIR)
	rm -f $(APP_NAME)
	rm -f $(APP_NAME).exe
	@echo "🧹 清理完成"
