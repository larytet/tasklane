GO ?= go
export TASKLANE_TEST_DATABASE_URL ?= postgres://tl:tl@localhost:55432/tasklane

.PHONY: build test vet db
build:
	$(GO) build -o bin/tasklane ./cmd/tasklane
vet:
	$(GO) vet ./...
test: vet
	$(GO) test ./... -count=1
# throwaway PostgreSQL for the test suite
db:
	docker run -d --name tasklane-pg -e POSTGRES_PASSWORD=tl -e POSTGRES_USER=tl -e POSTGRES_DB=tasklane -p 55432:5432 postgres:16 -c fsync=off

# 100,000-issue search benchmark and plan-shape assertions (slow to load)
perf:
	TASKLANE_PERF=1 $(GO) test ./internal/search -run 'PERF|TQL21' -count=1 -v -timeout 30m
	TASKLANE_PERF=1 $(GO) test ./internal/api -run 'TwoGiB|PERF_Server' -count=1 -v -timeout 30m
