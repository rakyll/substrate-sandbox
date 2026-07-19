.PHONY: build install test vet clean

build:
	go build ./...
	go build -o bin/sbcli ./cmd/sbcli
	go build -o bin/substrate-sandboxd ./cmd/substrate-sandboxd

# Install sbcli and substrate-sandboxd to $GOBIN (or $GOPATH/bin).
install:
	go install github.com/rakyll/substrate-sandbox/cmd/sbcli github.com/rakyll/substrate-sandbox/cmd/substrate-sandboxd

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
