APP := gonet
MAIN := ./cmd/gonet
DIST := dist
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all test build clean release release-all \
	release-linux-amd64 release-linux-arm64 release-windows-amd64 release-windows-arm64 \
	release-darwin-amd64 release-darwin-arm64

all: test build

test:
	go test ./...

build:
	go build -ldflags "$(LDFLAGS)" -o $(APP)$(shell go env GOEXE) $(MAIN)

clean:
	rm -rf $(DIST)

release: clean release-all

release-all: \
	release-linux-amd64 \
	release-linux-arm64 \
	release-windows-amd64 \
	release-windows-arm64 \
	release-darwin-amd64 \
	release-darwin-arm64

release-linux-amd64:
	@$(MAKE) package GOOS=linux GOARCH=amd64

release-linux-arm64:
	@$(MAKE) package GOOS=linux GOARCH=arm64

release-windows-amd64:
	@$(MAKE) package GOOS=windows GOARCH=amd64

release-windows-arm64:
	@$(MAKE) package GOOS=windows GOARCH=arm64

release-darwin-amd64:
	@$(MAKE) package GOOS=darwin GOARCH=amd64

release-darwin-arm64:
	@$(MAKE) package GOOS=darwin GOARCH=arm64

package:
	@mkdir -p $(DIST)
	@OUT_NAME=$(APP)-$(VERSION)-$(GOOS)-$(GOARCH); \
	BIN_NAME=$(APP); \
	if [ "$(GOOS)" = "windows" ]; then BIN_NAME=$${BIN_NAME}.exe; fi; \
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o "$(DIST)/$${OUT_NAME}/$${BIN_NAME}" $(MAIN); \
	cp README.md "$(DIST)/$${OUT_NAME}/README.md"; \
	if [ "$(GOOS)" = "windows" ]; then \
		(cd $(DIST) && zip -qr "$${OUT_NAME}.zip" "$${OUT_NAME}"); \
	else \
		tar -C $(DIST) -czf "$(DIST)/$${OUT_NAME}.tar.gz" "$${OUT_NAME}"; \
	fi; \
	rm -rf "$(DIST)/$${OUT_NAME}"
