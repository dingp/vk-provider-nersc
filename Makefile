BINARY_NAME=vk-nersc

all: build

tidy:
	go mod tidy

build:
	go build -o bin/$(BINARY_NAME) ./cmd/vk-nersc

build-probe:
	go build -o bin/sfapi-probe ./cmd/sfapi-probe

test:
	go test ./...

run:
	SF_API_ENDPOINT=https://api.nersc.gov/api/v1.2 \
	VK_NODE_NAME=perlmutter-vk \
	./bin/$(BINARY_NAME)

clean:
	rm -rf bin
