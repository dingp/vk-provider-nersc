FROM golang:1.21 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build

FROM debian:bullseye-slim
WORKDIR /app
COPY --from=builder /app/bin/vk-nersc /usr/local/bin/vk-nersc
ENTRYPOINT ["vk-nersc"]
