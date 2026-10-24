BINARY := loadtest
PKG    := ./cmd/loadtest
BUILD  := build
# Strip debug info for smaller binaries.
LDFLAGS := -s -w

.PHONY: all build install test vet fmt clean release

all: build

## build: compile the binary for the host platform into build/
build:
	@mkdir -p $(BUILD)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD)/$(BINARY) $(PKG)
	@echo "built $(BUILD)/$(BINARY)"

## install: install the binary into $GOBIN / $GOPATH/bin
install:
	go install -ldflags "$(LDFLAGS)" $(PKG)

## test: run unit tests
test:
	go test ./...

## vet: run go vet
vet:
	go vet ./...

## fmt: format all sources
fmt:
	go fmt ./...

## clean: remove build artifacts
clean:
	rm -rf $(BUILD)

## release: cross-compile static binaries for common platforms.
## A target that the local toolchain cannot build (e.g. a broken distro Go
## that lacks darwin/arm64 runtime assembly) is skipped with a warning rather
## than aborting the whole release.
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
release:
	@mkdir -p $(BUILD)
	@for t in $(RELEASE_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out=$(BUILD)/$(BINARY)-$$os-$$arch; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		if GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $$out $(PKG) 2>/tmp/lt_build_err; then \
			echo "built $$out"; \
		else \
			echo "WARNING: skipped $$os/$$arch (toolchain cannot build this target):"; \
			sed 's/^/    /' /tmp/lt_build_err; \
		fi; \
	done
	@echo "release binaries in $(BUILD)/"
