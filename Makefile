# Go 环境覆盖（可选）：仓库根目录若有 local.mk 就会被包含进来。
#
# 默认不设置任何东西——CI 与普通开发机上 Go 自己的默认值就是对的。
# 只有默认缓存位置不可写的环境才需要 local.mk，见 local.mk.example。
#
# 这里曾经把路径硬编码成 $(CURDIR)/../.gotmp，本地能用，CI 上因为那个目录
# 不存在直接让 `go vet` 挂掉（Go 不会自动创建 GOTMPDIR）。环境相关的绕行
# 手段属于本机，不属于项目。
-include local.mk

# 若外部指定了这些路径，顺手建出来——Go 只创建 GOCACHE，不创建 GOTMPDIR。
ifneq ($(strip $(GOTMPDIR)),)
$(shell mkdir -p "$(GOCACHE)" "$(GOPATH)" "$(GOTMPDIR)" 2>/dev/null)
endif

export GOCACHE
export GOPATH
export GOTMPDIR
export GOPROXY

# 发布版本号：默认取 git describe，release 工作流会用 VERSION= 显式覆盖。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build release test vet fmt fmt-check web-check e2e ci clean clean-dist

all: fmt-check vet test build

build:
	@mkdir -p bin
	go build -o bin/server ./cmd/server
	go build -o bin/agent ./cmd/agent
	@ls -la bin

# 交叉编译 + 打包 + 校验和。本地与 release 工作流共用这一份逻辑，
# "发布物怎么打出来"只有一处定义。
release:
	VERSION=$(VERSION) ./scripts/build-release.sh

test:
	go test -cover ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# CI 用的格式检查：有未格式化的文件就失败，而不是顺手改掉。
fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "以下文件未格式化，请先跑 make fmt："; echo "$$unformatted"; exit 1; \
	fi; \
	echo "gofmt: 干净"

web-check:
	./scripts/check-web.sh

# 端到端：起 server + agent，验证落库、在线判定、在线率、曲线与加机器流程。
e2e:
	./scripts/e2e.sh

# 与 .github/workflows/ci.yml 做同一套事，本地一条命令复现 CI。
ci: fmt-check vet test web-check build e2e

clean:
	rm -rf bin .e2e .demo

clean-dist:
	rm -rf dist
