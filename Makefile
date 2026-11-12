GO ?= go
DATABASE_URL ?= postgres://postgres:postgres@localhost:5433/orders_go
TEST_DATABASE_URL ?= postgres://postgres:postgres@localhost:5433/orders_go_test?sslmode=disable

.PHONY: build test vet fmt run migrate seed bench docker

build:
	$(GO) build ./...

test:
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' $(GO) test $(if $(RACE),-race,) ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

run:
	DATABASE_URL='$(DATABASE_URL)' $(GO) run ./cmd/api

migrate:
	DATABASE_URL='$(DATABASE_URL)' $(GO) run ./cmd/migrate

seed:
	DATABASE_URL='$(DATABASE_URL)' $(GO) run ./cmd/seed

bench:
	DATABASE_URL='$(DATABASE_URL)' $(GO) run ./cmd/bench

docker:
	docker compose up --build
