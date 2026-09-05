.PHONY: fmt-check test vet build image image-check container-test live-acceptance check

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

live-acceptance:
	@case "$(MODE)" in \
		fresh) \
			test -n "$(IMAGE)" || { printf '%s\n' 'IMAGE is required' >&2; exit 64; }; \
			exec bash hack/live-acceptance.sh fresh "$(IMAGE)"; \
			;; \
		migrate) \
			test -n "$(IMAGE)" || { printf '%s\n' 'IMAGE is required' >&2; exit 64; }; \
			test -n "$(SOURCE_SERVER_DIR)" || { printf '%s\n' 'SOURCE_SERVER_DIR is required' >&2; exit 64; }; \
			test -n "$(SOURCE_DATA_DIR)" || { printf '%s\n' 'SOURCE_DATA_DIR is required' >&2; exit 64; }; \
			exec bash hack/live-acceptance.sh migrate "$(IMAGE)" "$(SOURCE_SERVER_DIR)" "$(SOURCE_DATA_DIR)"; \
			;; \
		*) \
			printf '%s\n' 'MODE must be fresh or migrate' >&2; \
			exit 64; \
			;; \
	esac

check: fmt-check test vet
