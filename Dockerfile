FROM golang:1.27.1@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190 AS build

WORKDIR /src

ARG LDFLAGS
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN make build \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    ${LDFLAGS:+LDFLAGS="${LDFLAGS}"}

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

ARG VERSION=dev
ARG VCS_REF=unknown

LABEL org.opencontainers.image.title="puppet-forge" \
      org.opencontainers.image.source="https://github.com/zxzharmlesszxz/puppet-forge" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}"

RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 forge \
    && adduser -S -D -H -u 10001 -G forge forge

COPY --from=build /src/dist/puppet-forge /usr/local/bin/puppet-forge

EXPOSE 8080 9090

USER 10001:10001

ENTRYPOINT ["/usr/local/bin/puppet-forge"]
