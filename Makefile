VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PYTHON  ?= python3

.PHONY: build test e2e lint dist clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o promptape ./cmd/promptape

test:
	go vet ./...
	go test -race -count=1 ./...

e2e:
	PYTHON=$(PYTHON) ./e2e/run.sh

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)
	go vet ./...

dist:
	rm -rf dist && mkdir -p dist
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; [ $$os = windows ] && ext=".exe"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/promptape_$${os}_$${arch}/promptape$$ext ./cmd/promptape || exit 1; \
		cp LICENSE README.md dist/promptape_$${os}_$${arch}/ || exit 1; \
		(cd dist && tar -czf promptape_$(VERSION)_$${os}_$${arch}.tar.gz promptape_$${os}_$${arch}); \
	done
	cd dist && sha256sum *.tar.gz > checksums.txt

clean:
	rm -rf promptape dist
