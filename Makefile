# 本机 Go 的构建缓存默认落在不可写的位置（/root/.cache 或 /go），
# 所以这里统一指向工作区。要改路径用 make GOCACHE=... 覆盖。
export GOCACHE  := $(CURDIR)/../.gocache
export GOPATH   := $(CURDIR)/../.gopath
export GOTMPDIR := $(CURDIR)/../.gotmp
export GOPROXY  ?= https://goproxy.cn,direct

.PHONY: all build test vet fmt fmt-check web-check e2e ci clean

all: fmt-check vet test build

build:
	@mkdir -p bin
	go build -o bin/server ./cmd/server
	go build -o bin/agent ./cmd/agent
	@ls -la bin

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
