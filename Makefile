.DEFAULT_GOAL := check

COMPOSE := docker compose -f compose.yml
CHECKS := hygiene verify-generated go-checks ui-checks chart-checks
WRITE_TASKS := generate format-go format-hygiene

export FALCON_UID := $(shell id -u)
export FALCON_GID := $(shell id -g)

# Keep checks and source-writing tasks sequential, including with make -j.
.NOTPARALLEL:
.PHONY: check format $(CHECKS) $(WRITE_TASKS)

check: $(CHECKS)

format: format-go format-hygiene

# Rebuild tool images when their definitions change; unchanged layers are cached.
$(CHECKS) $(WRITE_TASKS):
	$(COMPOSE) build $@
	$(COMPOSE) run --rm --no-deps -T $@

# Default CI E2E is an installation smoke test. The full sync/publish scenario
# is opt-in; both run in kind and print diagnostics before cluster cleanup.
e2e e2e-full:
	$(COMPOSE) build e2e
	docker build -t falcon:e2e -f Dockerfile .
	$(COMPOSE) run --rm --no-deps -T e2e $@
.PHONY: e2e e2e-full
