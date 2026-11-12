VERSION ?= 0.5.0
LDFLAGS := -s -w -X github.com/continuum-dev/continuum/internal/core.Version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: build test integration real-agents dist vet docs clean

HOST := $(shell go env GOOS)-$(shell go env GOARCH)

# Builds dist/continuum-<os>-<arch> for this computer and points the
# ./continuum link at it.
build:
	@mkdir -p dist
	go build -ldflags '$(LDFLAGS)' -o dist/continuum-$(HOST) ./cmd/continuum
	@ln -sfn dist/continuum-$(HOST) continuum

test:
	go test ./...

vet:
	go vet ./...
	GOOS=linux go vet ./...
	GOOS=windows go vet ./... 2>&1 | grep -v 'possible misuse of unsafe.Pointer' || true

docs:
	scripts/gen-docs.sh

integration:
	scripts/integration-test.sh

real-agents:
	go test -tags realagents ./internal/deliver -run Real -v -count=1

dist:
	@mkdir -p dist
	@[ -e continuum ] || ln -sfn dist/continuum-$(HOST) continuum
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "dist/continuum-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' -o dist/continuum-$$os-$$arch$$ext ./cmd/continuum || exit 1; \
	done

clean:
	rm -rf continuum dist
