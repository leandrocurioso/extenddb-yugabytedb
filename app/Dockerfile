FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /users-api .

FROM alpine:3.22
# Runs as ExtendDB's UID so it can read the cell's TLS cert from the shared state volume.
USER 10001:10001
COPY --from=build /users-api /usr/local/bin/users-api
ENV GIN_MODE=release
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/users-api"]
