BINARY_NAME=vk-nersc

all: build

build:
	go build -o bin/$(BINARY_NAME) ./cmd/vk-nersc

run:
	SF_API_ENDPOINT=https://api.nersc.gov/api/v1.2 \
	SF_API_TOKEN=$(SF_API_TOKEN) \
	VK_NODE_NAME=perlmutter-vk \
	./bin/$(BINARY_NAME)

clean:
	rm -rf bin
