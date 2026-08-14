FROM golang:1.26.6@sha256:640a234f4bea3e399c056b7b8f9c667c4939befae8db2f14e9785e16eccd4205 AS build

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

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 forge

COPY --from=build /src/dist/puppet-forge /usr/local/bin/puppet-forge

EXPOSE 8080 9090

USER forge

ENTRYPOINT ["/usr/local/bin/puppet-forge"]
