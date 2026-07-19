.PHONY: build install test vet clean

build:
	go build ./...
	go build -o bin/ssbx ./cmd/ssbx
	go build -o bin/substrate-sandbox ./cmd/substrate-sandbox

# Install ssbx and substrate-sandbox to $GOBIN (or $GOPATH/bin).
install:
	go install github.com/rakyll/substrate-sandbox/cmd/ssbx github.com/rakyll/substrate-sandbox/cmd/substrate-sandbox

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
