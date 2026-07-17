.PHONY: build run test test-race vet web web-check openapi openapi-check skill-check integration compose-up compose-down verify

build: web
	mkdir -p bin
	go build -trimpath -o bin/manifold ./cmd/manifold

run: web
	go run ./cmd/manifold

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

web:
	npm run build

web-check:
	npm run check

openapi: web
	mkdir -p api
	go run ./cmd/manifold openapi > api/openapi.yaml

openapi-check: web
	@tmp=$$(mktemp); go run ./cmd/manifold openapi > $$tmp; diff -u api/openapi.yaml $$tmp; rm -f $$tmp

skill-check:
	python3 scripts/validate-skill.py skills/manifold
	go test ./skills/manifold/cmd/manifold
	skills/manifold/scripts/manifold --version

integration:
	scripts/integration.sh

compose-up:
	docker compose up -d --build

compose-down:
	docker compose down

verify: test test-race vet web-check web openapi-check skill-check
