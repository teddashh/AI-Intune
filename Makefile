# clawctl
#
# Go 裝在 ~/.local/go（不動系統）。如果你的 go 在 PATH 上，這行會自動略過。
GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/.local/go/bin/go)

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# ⚠ CGO_ENABLED=0 是硬性要求，不是偏好。
#
# agent 要去升級 Node 寫的 CLI，它不能跟被它管的東西共用失敗域。
# 一個 static binary 在一台 glibc 被搞壞的機器上仍然跑得起來 ——
# 而那正好是你最需要它回報的時候。
# sqlite 用 modernc.org/sqlite（純 Go），就是為了這個。
BUILD := CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)"

.PHONY: all build hub agent agent-bundles agent-bundles-darwin test test-go test-ops vet fmt clean probe cross cross-darwin

all: build

build: hub agent

hub:
	@mkdir -p build
	$(BUILD) -o build/clawctl-hub ./cmd/clawctl-hub

agent:
	@mkdir -p build
	$(BUILD) -o build/clawctl-agent ./cmd/clawctl-agent

# 機隊裡有 x86_64 也有 aarch64（兩台 Oracle ARM + sampleagent3）。
cross:
	@mkdir -p build
	GOOS=linux GOARCH=amd64 $(BUILD) -o build/clawctl-agent-linux-amd64 ./cmd/clawctl-agent
	GOOS=linux GOARCH=arm64 $(BUILD) -o build/clawctl-agent-linux-arm64 ./cmd/clawctl-agent
	@ls -la build/clawctl-agent-linux-*

# macOS bundle 使用獨立架構 binary，並帶入 installer 與 LaunchAgent plist。
cross-darwin:
	@mkdir -p build
	GOOS=darwin GOARCH=arm64 $(BUILD) -o build/clawctl-agent-darwin-arm64 ./cmd/clawctl-agent
	GOOS=darwin GOARCH=amd64 $(BUILD) -o build/clawctl-agent-darwin-amd64 ./cmd/clawctl-agent
	@ls -la build/clawctl-agent-darwin-*

agent-bundles: cross
	./ops/build-agent-bundles.sh "$(VERSION)"

agent-bundles-darwin: cross-darwin
	./ops/build-agent-bundles.sh "$(VERSION)" darwin

# ⚠ test 一定要同時跑 Go 跟 ops/。
#
# ops/*.sh 承載了整條告警鏈的判斷，而它的測試如果要靠人記得手動跑，
# 那它就等於沒有 —— 一支沒有人跑的測試跟一支沒寫的測試，防護力一樣。
test: test-go test-ops

test-go:
	$(GO) test ./...

# ⚠ 不連網、不碰 ~/.config、不碰任何真的機器。理由見 ops/test-ops.sh 開頭。
test-ops:
	./ops/test-ops.sh

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# 在本機跑一次唯讀觀測，看它實際量到什麼。
probe: agent
	./build/clawctl-agent probe --pretty

clean:
	rm -rf build
