.PHONY: build install test vet clean

build:
	go build ./...
	go build -o bin/sbcli ./cmd/sbcli
	go build -o bin/substrate-sandbox ./cmd/substrate-sandbox

# Install sbcli and substrate-sandbox to $GOBIN (or $GOPATH/bin).
install:
	go install github.com/rakyll/substrate-sandbox/cmd/sbcli github.com/rakyll/substrate-sandbox/cmd/substrate-sandbox

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
