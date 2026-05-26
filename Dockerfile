FROM golang:1.22-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod ./
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/runner ./cmd/runner

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
  && adduser -D -h /work runner \
  && mkdir -p /work \
  && chown -R runner:runner /work

WORKDIR /work

COPY --from=build /out/runner /usr/local/bin/runner

USER runner

ENTRYPOINT ["/usr/local/bin/runner"]
CMD ["--run"]
