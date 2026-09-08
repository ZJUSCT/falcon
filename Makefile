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
