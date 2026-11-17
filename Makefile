VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

FUZZTIME ?= 30s

.PHONY: build test lint fmt vet tidy clean fuzz

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o powerz ./cmd/powerz

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

# Fuzz every target (untrusted meter / USB-PD input) for FUZZTIME each.
fuzz:
	./scripts/fuzz.sh $(FUZZTIME)

lint:
	golangci-lint run

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -rf powerz dist
