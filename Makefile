.PHONY: fmt-check test vet build image check

fmt-check:
	@test -z "$$(gofmt -l .)"

test:
	go test ./...

vet:
	go vet ./...

build:
	go build -o vrisingctl .

image:
	docker build -t docker-vrising .

check: fmt-check test vet
