VERSION ?= 0.17.1
LDFLAGS := -s -w -X github.com/continuum-dev/continuum/internal/core.Version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: build test integration real-agents dist vet docs clean

HOST := $(shell go env GOOS)-$(shell go env GOARCH)
DIST_DIR ?= dist

# Builds $(DIST_DIR)/continuum-<os>-<arch> for this computer (and links ./continuum when DIST_DIR=dist).
build:
	@mkdir -p "$(DIST_DIR)"
	go build -trimpath -ldflags '$(LDFLAGS)' -o "$(DIST_DIR)/continuum-$(HOST)" ./cmd/continuum
	@if [ "$(DIST_DIR)" = "dist" ]; then ln -sfn "dist/continuum-$(HOST)" continuum; fi
	@echo "$(DIST_DIR)/continuum-$(HOST)"

test:
	go test ./...

vet:
	GOOS=darwin go vet ./...
	GOOS=linux go vet ./...
	GOOS=windows go vet ./...
	GOOS=freebsd go vet ./...

docs:
	scripts/gen-docs.sh

integration:
	scripts/integration-test.sh

real-agents:
	go test -tags realagents ./internal/deliver -run Real -v -count=1

dist:
	@mkdir -p "$(DIST_DIR)"
	@if [ "$(DIST_DIR)" = "dist" ]; then [ -e continuum ] || ln -sfn "dist/continuum-$(HOST)" continuum; fi
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "$(DIST_DIR)/continuum-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o "$(DIST_DIR)/continuum-$$os-$$arch$$ext" ./cmd/continuum || exit 1; \
	done

clean:
	rm -rf continuum dist
