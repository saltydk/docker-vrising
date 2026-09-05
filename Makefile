.PHONY: fmt-check test vet build image image-check container-test live-acceptance-test live-acceptance check

export LIVE_ACCEPTANCE_MODE := $(value MODE)
export LIVE_ACCEPTANCE_IMAGE := $(value IMAGE)
export LIVE_ACCEPTANCE_SOURCE_SERVER_DIR := $(value SOURCE_SERVER_DIR)
export LIVE_ACCEPTANCE_SOURCE_DATA_DIR := $(value SOURCE_DATA_DIR)

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

live-acceptance-test:
	bash hack/live-acceptance-test.sh

live-acceptance:
	@case "$${LIVE_ACCEPTANCE_MODE-}" in \
		fresh) \
			test -n "$${LIVE_ACCEPTANCE_IMAGE-}" || { printf '%s\n' 'IMAGE is required' >&2; exit 64; }; \
			exec bash hack/live-acceptance.sh fresh "$${LIVE_ACCEPTANCE_IMAGE}"; \
			;; \
		migrate) \
			test -n "$${LIVE_ACCEPTANCE_IMAGE-}" || { printf '%s\n' 'IMAGE is required' >&2; exit 64; }; \
			test -n "$${LIVE_ACCEPTANCE_SOURCE_SERVER_DIR-}" || { printf '%s\n' 'SOURCE_SERVER_DIR is required' >&2; exit 64; }; \
			test -n "$${LIVE_ACCEPTANCE_SOURCE_DATA_DIR-}" || { printf '%s\n' 'SOURCE_DATA_DIR is required' >&2; exit 64; }; \
			exec bash hack/live-acceptance.sh migrate "$${LIVE_ACCEPTANCE_IMAGE}" "$${LIVE_ACCEPTANCE_SOURCE_SERVER_DIR}" "$${LIVE_ACCEPTANCE_SOURCE_DATA_DIR}"; \
			;; \
		*) \
			printf '%s\n' 'MODE must be fresh or migrate' >&2; \
			exit 64; \
			;; \
	esac

check: fmt-check test vet live-acceptance-test
