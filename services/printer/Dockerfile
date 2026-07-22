FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/printer .

# Phase 10 (real printing): cups-client provides `lp`, which print.go shells
# out to when DEV_MODE=false. In dev mode `lp` is never called, but shipping it
# in every image keeps one artifact for both modes.
FROM alpine:3.19
RUN apk add --no-cache cups-client
COPY --from=build /out/printer /usr/local/bin/printer
EXPOSE 8444
ENTRYPOINT ["printer"]
