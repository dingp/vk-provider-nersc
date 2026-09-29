ARG GO_IMAGE=golang:1.21
ARG RUNTIME_IMAGE=debian:bullseye-slim
FROM ${GO_IMAGE} AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 make build build-probe

FROM ${RUNTIME_IMAGE}
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/bin/vk-nersc /usr/local/bin/vk-nersc
COPY --from=builder /app/bin/sfapi-probe /usr/local/bin/sfapi-probe
ENTRYPOINT ["vk-nersc"]
