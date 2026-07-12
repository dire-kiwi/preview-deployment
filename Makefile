COMPOSE := docker compose
DEV_COMPOSE := $(COMPOSE) -f compose.yaml -f compose.dev.yaml

.PHONY: up up-release pull down logs test example-zip deploy-example clean

up:
	$(DEV_COMPOSE) up --build -d

up-release: pull
	$(COMPOSE) up -d

pull:
	$(COMPOSE) pull

down:
	$(DEV_COMPOSE) down

logs:
	$(DEV_COMPOSE) logs -f traefik orchestrator

test:
	go test ./...

example-zip:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux go build -trimpath -o dist/app ./examples/hello
	rm -f dist/hello.zip
	cd dist && zip -q -j hello.zip app ../examples/hello/preview.json

# The direct port avoids depending on wildcard localhost DNS for API access.
deploy-example: example-zip
	curl --fail-with-body --form archive=@dist/hello.zip http://127.0.0.1:$${ORCHESTRATOR_PORT:-8081}/v1/deployments
	@printf '\n'

clean:
	rm -rf dist
