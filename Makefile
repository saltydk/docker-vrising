.PHONY: fmt-check test vet build image image-check check

fmt-check:
	@test -z "$$(gofmt -l .)"

test:
	go test ./...

vet:
	go vet ./...

build:
	go build -o vrisingctl .

image:
	docker buildx build --platform linux/amd64 --load -t saltydk/vrising:local .

image-check: image
	scripts/image-contract.sh saltydk/vrising:local

check: fmt-check test vet
