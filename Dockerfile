# syntax=docker/dockerfile:1.7

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build

ARG VERSION
ARG REVISION
ARG BUILD_DATE
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN test "$TARGETOS/$TARGETARCH" = "linux/amd64" && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -buildid=${REVISION}-${BUILD_DATE} -X main.buildVersion=${VERSION}" \
      -o /out/vrisingctl .

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS fixture-build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY testdata/container/sidecar/main.go ./main.go
RUN test "$TARGETOS/$TARGETARCH" = "linux/amd64" && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/fixture-sidecar ./main.go && \
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
      -subj /CN=docker-vrising-fixture-ca \
      -keyout /out/ca.key -out /out/ca.crt && \
    openssl req -newkey rsa:2048 -nodes -subj /CN=thunderstore.io \
      -keyout /out/server.key -out /out/server.csr && \
    printf '%s\n' 'subjectAltName=DNS:thunderstore.io' > /out/server.ext && \
    openssl x509 -req -days 2 -in /out/server.csr \
      -CA /out/ca.crt -CAkey /out/ca.key -CAcreateserial \
      -extfile /out/server.ext -out /out/server.crt

FROM ubuntu:22.04@sha256:2edbbc5dc405e9612ba3584ce95480277e3eb374407b5505fe26f17df77c7dbc AS production

ARG DEBIAN_FRONTEND=noninteractive

RUN dpkg --add-architecture i386 && \
    apt-get update && \
    apt-get install --yes --no-install-recommends \
      ca-certificates \
      curl \
      gnupg \
      software-properties-common && \
    add-apt-repository --yes multiverse && \
    install -d -m 0755 /etc/apt/keyrings && \
    curl --fail --silent --show-error --location \
      --output /etc/apt/keyrings/winehq-archive.key \
      https://dl.winehq.org/wine-builds/winehq.key && \
    curl --fail --silent --show-error --location \
      --output /etc/apt/sources.list.d/winehq-jammy.sources \
      https://dl.winehq.org/wine-builds/ubuntu/dists/jammy/winehq-jammy.sources && \
    printf '%s\n' 'steam steam/question select I AGREE' 'steam steam/license note' | debconf-set-selections && \
    apt-get update && \
    apt-get install --yes --install-recommends \
      steamcmd \
      winehq-stable && \
    apt-get install --yes --no-install-recommends \
      ca-certificates \
      gnupg \
      tini \
      tzdata \
      winbind \
      xvfb && \
    ln -s /usr/games/steamcmd /usr/local/bin/steamcmd && \
    if ! command -v wine64 >/dev/null 2>&1; then ln -s /opt/wine-stable/bin/wine /usr/local/bin/wine64; fi && \
    apt-get purge --yes curl software-properties-common && \
    apt-get autoremove --yes && \
    rm -rf /var/lib/apt/lists/* /var/cache/debconf/*-old

ARG VERSION
ARG REVISION
ARG BUILD_DATE

LABEL org.opencontainers.image.source="https://github.com/saltydk/docker-vrising" \
      org.opencontainers.image.revision="$REVISION" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.created="$BUILD_DATE" \
      org.opencontainers.image.description="Runtime control plane for installing and running the V Rising dedicated server"

COPY --from=build /out/vrisingctl /usr/local/bin/vrisingctl

VOLUME ["/mnt/vrising/server", "/mnt/vrising/persistentdata"]
EXPOSE 9876/udp 9877/udp 25575/tcp

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/vrisingctl"]
CMD ["run"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30m --retries=3 CMD ["/usr/local/bin/vrisingctl", "health"]

FROM production AS fixture

COPY --from=fixture-build /out/fixture-sidecar /usr/local/bin/fixture-sidecar
COPY --from=fixture-build /out/ca.crt /usr/local/share/ca-certificates/docker-vrising-fixture-ca.crt
COPY --from=fixture-build /out/server.crt /fixture/tls/server.crt
COPY --from=fixture-build /out/server.key /fixture/tls/server.key
COPY testdata/container/bin/ /fixture/bin/
RUN update-ca-certificates && chmod 0755 /fixture/bin/* /usr/local/bin/fixture-sidecar

ENV PATH="/fixture/bin:${PATH}"

FROM production AS final
