GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

.PHONY: help build test race vet docker docker-up snapshot clean

help:
	@echo "make build      编译到 ./bin/devpilot-relay"
	@echo "make test       单元测试"
	@echo "make race       单元测试（-race）"
	@echo "make docker     构建本地镜像"
	@echo "make docker-up  在 deploy/ 下启动 compose"
	@echo "make snapshot   导出 bbolt 快照到 ./bin/snapshot.db"

build:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/devpilot-relay ./cmd/relay

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

docker:
	docker build -f deploy/Dockerfile -t devpilot-relay:$(VERSION) .

docker-up:
	cd deploy && docker compose up -d --build

snapshot: build
	DATA_DIR=./bin/data ./bin/devpilot-relay snapshot --out ./bin/snapshot.db

clean:
	rm -rf bin dist
