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

# End-to-end scenario on a kind cluster: not part of `check`. Builds the
# controller image on the host, brings up the cluster through the e2e-tools
# container (Docker socket), asserts the demo Mirror flow, tears everything
# down. On failure the cluster state dumps straight into the output.
e2e:
	$(COMPOSE) build e2e
	docker build -t falcon:e2e -f Dockerfile .
	$(COMPOSE) run --rm --no-deps -T e2e
.PHONY: e2e
