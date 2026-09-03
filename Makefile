
.PHONY: build test vet lint tidy clean run e2e e2e-linux e2e-systemd

BIN := bin/reloop
PKG := ./...

build:
	@mkdir -p bin
	go build -o $(BIN) ./cmd/reloop

test:
	go test -race -count=1 $(PKG)

vet:
	go vet $(PKG)

# Pinned and built from source so the linter can never lag the go.mod
# Go version, and everyone lints with the same release as CI.
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 run --timeout=5m

tidy:
	go mod tidy

clean:
	rm -rf bin

run: build
	$(BIN)

# End-to-end harness: installs the real binary and drives the CLI.
e2e:
	bash tests/e2e/run.sh

e2e-linux:
	docker build -f tests/e2e/Dockerfile -t reloop-e2e .
	docker run --rm reloop-e2e

# Same harness inside a booted-systemd container, so 'reloop install'
# runs against a real systemd --user manager.
e2e-systemd:
	bash tests/e2e/systemd.sh
