BUCKET_NAME ?=

.PHONY: build install test vet deploy clean

build:
	go build ./...
	go build -o bin/sbcli ./cmd/sbcli
	go build -o bin/substrate-sandboxd ./cmd/substrate-sandboxd

# Install sbcli and substrate-sandboxd to $GOBIN (or $GOPATH/bin).
install:
	go install ./cmd/sbcli ./cmd/substrate-sandboxd

test:
	go test ./...

vet:
	go vet ./...

# Deploy the WorkerPool + ActorTemplate and build/push the guest image
# with ko. Requires KO_DOCKER_REPO and BUCKET_NAME to be set.
deploy:
	@test -n "$(BUCKET_NAME)" || (echo "set BUCKET_NAME to the snapshots bucket" && exit 1)
	BUCKET_NAME=$(BUCKET_NAME) envsubst < manifests/sandbox.yaml.tmpl | ko apply -f -

clean:
	rm -rf bin
