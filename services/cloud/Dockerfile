FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /out/cloud-server .

FROM alpine:3.19
COPY --from=build /out/cloud-server /usr/local/bin/cloud-server
EXPOSE 8080
ENTRYPOINT ["cloud-server"]
