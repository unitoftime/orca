BINARY := build/orca

.PHONY: build test fmt vet clean install pin-images

# Static, so the same binary runs on the machines: apply ships it there for the
# status page, into an image whose C library is not this machine's. Go records
# the commit it was built from, which is what `orca version` prints.
build:
	@mkdir -p build
	CGO_ENABLED=0 go build -o $(BINARY) ./cmd/orca

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
