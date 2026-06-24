FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/cloud-server .

FROM alpine:3.19
RUN apk add --no-cache curl
COPY --from=build /out/cloud-server /usr/local/bin/cloud-server
EXPOSE 8080 8443
ENTRYPOINT ["cloud-server"]
