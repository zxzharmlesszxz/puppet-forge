FROM golang:1.27.0@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146 AS build

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

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

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
