GO ?= go
export TASKLANE_TEST_DATABASE_URL ?= postgres://tl:tl@localhost:55432/tasklane

.PHONY: build test vet db notice dist perf e2e e2e-update interop interop-matrix upgrade
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
	TASKLANE_PERF=1 $(GO) test ./internal/domain -run 'MAIL17_PERF' -count=1 -v -timeout 30m

# regenerate NOTICE (licences of every linked module); a test fails when it is stale
notice:
	$(GO) run ./cmd/notice

# reproducible release tarballs for linux/amd64 and linux/arm64, with SHA256SUMS and an SBOM
dist:
	GO=$(GO) ./scripts/dist.sh dist

# browser end-to-end suite (system Chrome, real binary, fresh database): conflicts, drafts, presence, axe, responsive, visual
e2e:
	cd e2e && npm ci --no-audit --no-fund && env -u TASKLANE_TEST_DATABASE_URL PATH=$$PATH:/usr/local/go/bin npx playwright test
e2e-update:
	cd e2e && env -u TASKLANE_TEST_DATABASE_URL PATH=$$PATH:/usr/local/go/bin npx playwright test --update-snapshots

# real-service interoperability smoke: two nodes with real PostgreSQL, Redis and OpenSearch from deploy/compose/docker-compose.interop.yml (needs Docker)
interop:
	./scripts/interop.sh

# every supported Redis or Valkey and OpenSearch combination (nightly)
interop-matrix:
	./scripts/interop-matrix.sh

# rolling upgrade from the previous release to this one under simulated load, two nodes behind a load balancer (needs Docker)
upgrade:
	./scripts/upgrade.sh
