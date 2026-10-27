GO ?= go
OUT ?= dist/deepseek-search.so

.PHONY: all build test race vet check clean

all: check build

build:
	mkdir -p $(dir $(OUT))
	$(GO) build -trimpath -buildmode=c-shared -o $(OUT) .

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: test race vet

clean:
	rm -rf dist
