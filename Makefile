BINARY  := codehamr
PKG     := ./cmd/codehamr
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX  ?= /usr/local

.PHONY: build run install loc

build:
	@mkdir -p bin
	@set -e; for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
	    os=$${t%/*}; arch=$${t#*/}; ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	    label=$$os; [ "$$os" = "darwin" ] && label="macos"; \
	    echo "→ $$os/$$arch"; \
	    GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
	        -o bin/$(BINARY)-$$label-$$arch$$ext $(PKG); \
	done

# A clean tree build carries a real version string, so the freshness check would
# "update" this local binary back to the last published release. Suppress it.
run: build
	@os=$$(go env GOOS); arch=$$(go env GOARCH); \
	 label=$$os; [ "$$os" = "darwin" ] && label=macos; \
	 ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	 CODEHAMR_NO_UPDATE_CHECK=1 ./bin/$(BINARY)-$$label-$$arch$$ext

install:
	@mkdir -p $(PREFIX)/bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(PREFIX)/bin/$(BINARY) $(PKG)
	@echo "→ installed $(PREFIX)/bin/$(BINARY) ($(VERSION))"

loc:
	@find . -type f -name '*.go' \
	    -not -path './bin/*' -not -path './.git/*' \
	    -exec wc -l {} + | tail -n 1 | awk '{print $$1 " lines of Go"}'
