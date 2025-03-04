
all: build push

docker-build-release:
	@if [ -z "$(tag)" ]; then \
		echo "Error: tag is required. Usage: make build-all tag=<tag>"; \
		exit 1; \
	fi
	docker buildx build --platform linux/arm64,linux/amd64 -t fosrl/gerbil:latest -f Dockerfile --push .
	docker buildx build --platform linux/arm64,linux/amd64 -t fosrl/gerbil:$(tag) -f Dockerfile --push .

build:
	docker build -t fosrl/gerbil:latest .

push:
	docker push fosrl/gerbil:latest

test:
	docker run -it -p 3002:3002 -v ./config_example.json:/config/config.json --cap-add=NET_ADMIN --cap-add=SYS_MODULE gerbil --config /config/config.json

local: 
	 CGO_ENABLED=0 GOOS=linux go build -o gerbil

go-build-release:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/gerbil_linux_arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/gerbil_linux_amd64

clean:
	rm gerbil
