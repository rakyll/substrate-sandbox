.PHONY: build install test vet clean

build:
	go build ./...
	go build -o bin/sbcli ./cmd/sbcli
	go build -o bin/substrate-sandbox-api ./cmd/substrate-sandbox-api

# Install sbcli and substrate-sandbox-api to $GOBIN (or $GOPATH/bin).
install:
	go install github.com/rakyll/substrate-sandbox/cmd/sbcli github.com/rakyll/substrate-sandbox/cmd/substrate-sandbox-api

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
