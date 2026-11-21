VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o ccmon .

test:
	go vet ./...
	go test ./...

# Every platform builds from any one of them; no C compiler is needed.
dist:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/ccmon-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/ccmon-linux-arm64 .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/ccmon-macos-amd64 .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/ccmon-macos-arm64 .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/ccmon-windows-amd64.exe .

clean:
	rm -rf ccmon ccmon.exe dist
