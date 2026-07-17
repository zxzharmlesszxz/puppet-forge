FROM golang:1.26.5 AS build

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

FROM alpine:3.24

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 forge

COPY --from=build /src/dist/puppet-forge /usr/local/bin/puppet-forge

EXPOSE 8080

USER forge

ENTRYPOINT ["/usr/local/bin/puppet-forge"]
