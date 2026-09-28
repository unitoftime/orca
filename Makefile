BINARY := build/orca

# Stamped into the binary so `orca version` can tell two builds apart. Without
# it every build says 0.0.1, and a stale `make install` is invisible.
BUILD := $(shell git describe --always --dirty 2>/dev/null || echo unknown)
LDFLAGS := -ldflags "-X main.build=$(BUILD)"

.PHONY: build test fmt vet clean install pin-images

# Static, so the same binary runs on the machines: apply ships it there for the
# status page, into an image whose C library is not this machine's.
build:
	@mkdir -p build
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BINARY) ./cmd/orca

test:
	go test ./...

fmt:
	gofmt -w ./cmd

vet:
	go vet ./...

# Re-resolves every image orca chooses itself to the digest its tag points at
# now. See pkg/images.
pin-images:
	go run ./internal/pinimages

install: build
	install -m 0755 $(BINARY) $(HOME)/.local/bin/orca

clean:
	rm -rf build
