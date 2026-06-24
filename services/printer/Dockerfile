FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/printer .

# Phase 3 is dev-mode only (no CUPS/lp call -- see print.go), so a bare
# alpine base is enough. Phase 6 (real printing) will need a base image
# with cups-client installed for the lp command.
FROM alpine:3.19
COPY --from=build /out/printer /usr/local/bin/printer
EXPOSE 8444
ENTRYPOINT ["printer"]
