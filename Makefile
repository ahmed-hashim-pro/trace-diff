# Thin wrappers over go, plus the targets that regenerate what the README shows.
GO ?= go
SHOP := examples/shop

.PHONY: build test check bench demo demo-test docs gif clean

build:
	$(GO) build -o trace-diff ./cmd/trace-diff

test:
	$(GO) vet ./...
	$(GO) test ./...
	cd demo && $(GO) vet ./... && $(GO) test ./...

check: test bench

# Every synthetic scenario over 200 seeds, and the broken comparisons.
bench: build
	./trace-diff bench --seeds 200 --mutants

# The real shop: Postgres, three services instrumented with the OpenTelemetry
# SDK and a Collector, run before the change, again unchanged, and after.
# Needs Docker. Writes demo/out/{before,rerun,after}.jsonl, then compares them.
demo: build
	demo/run.sh
	./trace-diff check demo/out/before.jsonl demo/out/rerun.jsonl
	./trace-diff check demo/out/before.jsonl demo/out/after.jsonl || true

# The shop's own tests, against the demo's Postgres (start it with
# `docker compose -f demo/compose.yaml up -d --wait postgres`).
demo-test:
	cd demo && DATABASE_URL=postgres://postgres:shop@127.0.0.1:15432/postgres $(GO) test -count=1 ./...

# Re-render docs/ from the committed example run.
docs: build
	./trace-diff check $(SHOP)/before.jsonl.gz $(SHOP)/rerun.jsonl.gz > docs/rerun.txt
	./trace-diff check $(SHOP)/before.jsonl.gz $(SHOP)/after.jsonl.gz --svg docs/n-plus-one.svg --svg-kind n-plus-one > docs/report.txt || true
	./trace-diff check $(SHOP)/before.jsonl.gz $(SHOP)/after.jsonl.gz --svg docs/retry-storm.svg --svg-kind retry-storm > /dev/null || true
	./trace-diff bench --seeds 200 --mutants > docs/bench.txt

# Re-record docs/demo.gif from the real CLI. Needs Docker (VHS); the binary
# is static, so it runs in that image.
gif:
	CGO_ENABLED=0 $(GO) build -o trace-diff ./cmd/trace-diff
	docker run --rm -u $$(id -u):$$(id -g) -e HOME=/tmp -v $(CURDIR):/vhs ghcr.io/charmbracelet/vhs:v0.10.0 docs/demo.tape

clean:
	rm -f trace-diff
