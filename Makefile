.PHONY: fmt-check test vet build image image-check container-test check

fmt-check:
	@test -z "$$(gofmt -l .)"

test:
	go test ./...

vet:
	go vet ./...

build:
	go build -o vrisingctl .

image:
	docker buildx build --platform linux/amd64 --target production --load -t saltydk/vrising:local .

image-check: image
	scripts/image-contract.sh saltydk/vrising:local

container-test:
	docker buildx build --platform linux/amd64 --target production --load -t docker-vrising:production-test .
	docker buildx build --platform linux/amd64 --load -t docker-vrising:default-test .
	docker buildx build --platform linux/amd64 --target fixture --load -t docker-vrising:fixture-test .
	PRODUCTION_IMAGE=docker-vrising:production-test DEFAULT_IMAGE=docker-vrising:default-test FIXTURE_IMAGE=docker-vrising:fixture-test bash hack/container-fixture-test.sh

check: fmt-check test vet
