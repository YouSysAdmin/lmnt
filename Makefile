BIN     := lmnt
MODULE  := github.com/yousysadmin/lmnt
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)

.PHONY: build test vet lint dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/lmnt

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run

## dist builds zipped macOS binaries into dist/ with a sha256 list.
PLATFORMS := darwin/arm64 darwin/amd64
dist: clean
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=$(BIN)_$${os}_$${arch}_$(VERSION); \
		echo "building $$name"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$name ./cmd/lmnt && \
		(cd dist && zip -q $$name.zip $$name && rm $$name); \
	done
	@cd dist && shasum -a 256 *.zip > $(BIN)_sha256_$(VERSION).txt && cat $(BIN)_sha256_$(VERSION).txt

clean:
	rm -rf dist $(BIN)
