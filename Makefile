PROFILES ?= cells monitoring
COMPOSE  := docker compose $(foreach p,$(PROFILES),--profile $(p))
CELLS   := 1 2 3

# Shared-database test configuration (docker-compose.shared.yml): the three
# cells serve one database. The compat and chaos tests run against it.
SHARED := docker compose -f docker-compose.shared.yml $(foreach p,$(PROFILES),--profile $(p))

# users-api of cell N listens on localhost:808N.
# $(call create_user,<cell>): POST a user to that cell. ID/NAME/EMAIL override
# the generated defaults; the id is unique per cell and per call unless ID is set.
define create_user
id="$(or $(ID),user-cell$(1)-$$(date +%s%N))"; \
echo "== cell$(1): creating $$id"; \
curl -fsS -X POST localhost:808$(1)/users -H 'Content-Type: application/json' \
  -d "{\"id\":\"$$id\",\"name\":\"$(or $(NAME),User $$id)\",\"email\":\"$(or $(EMAIL),$$id@example.com)\"}"; echo
endef

K6_IMAGE ?= grafana/k6:2.3.0
LOAD_CELL ?= 1
TPS       ?= 100
DURATION  ?= 2m

# k6 for the compat/chaos tests: scripts from loadtest/, reports written to
# reports/ as the calling user, metrics pushed to Prometheus when it is up.
RUN_ID ?= $(shell date +%m%d%H%M%S)
K6_TEST = docker run --rm -i --network host --user $$(id -u):$$(id -g) \
  -v $(CURDIR)/loadtest:/scripts:ro -v $(CURDIR)/reports:/reports \
  -e K6_PROMETHEUS_RW_SERVER_URL=http://localhost:9090/api/v1/write -e K6_PROMETHEUS_RW_TREND_STATS \
  -e RUN_ID=$(RUN_ID) $(K6_IMAGE) run -o experimental-prometheus-rw
# Passed through the environment: chaos/run.sh receives K6_TEST as a string,
# where the parentheses could not be quoted.
export K6_PROMETHEUS_RW_TREND_STATS := p(95),p(99),min,max,avg,med
CHAOS_SCENARIOS := app-kill extenddb-kill yb-kill yb-pause yb-partition yb-rolling-restart yb-quorum-loss cluster-restart

check-cell = case "$(CELL)" in 1|2|3) ;; *) echo "CELL must be one of: $(CELLS)" >&2; exit 2;; esac

.PHONY: help build up down clean ps logs smoke user-create user-create-all user-get load \
  shared-up shared-down shared-clean shared-ps shared-logs shared-smoke \
  compat consistency chaos chaos-all test-all

help: ## Show targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

build: ## Build the patched ExtendDB image and the users-api image
	$(COMPOSE) build extenddb-cell1 users-api-cell1

up: build ## Start all cells + Prometheus/Grafana (PROFILES="cells monitoring")
	$(COMPOSE) up -d --wait

down: ## Stop all cells, keeping data
	$(COMPOSE) down

clean: ## Stop all cells and delete all data volumes
	$(COMPOSE) down -v

ps: ## Show service status
	$(COMPOSE) ps -a

logs: ## Follow logs (SERVICE=<name> to filter)
	$(COMPOSE) logs -f $(SERVICE)

smoke: ## Write and read a user through each cell
	@set -e; for c in $(CELLS); do \
	  port=808$$c; \
	  echo "== cell$$c (:$$port)"; \
	  curl -fsS localhost:$$port/health; echo; \
	  curl -fsS -X POST localhost:$$port/users -H 'Content-Type: application/json' \
	    -d "{\"id\":\"user-$$c\",\"name\":\"User $$c\",\"email\":\"user$$c@example.com\"}"; echo; \
	  curl -fsS localhost:$$port/users/user-$$c; echo; \
	done

user-create: ## Create a user in one cell: CELL=<1|2|3> [ID=..] [NAME=..] [EMAIL=..]
	@$(check-cell); $(call create_user,$(CELL))

user-create-all: ## Create a new user in every cell: [ID=..] [NAME=..] [EMAIL=..]
	@set -e; $(foreach c,$(CELLS),$(call create_user,$(c));)

user-get: ## Read a user from one cell: CELL=<1|2|3> ID=<id>
	@$(check-cell); test -n "$(ID)" || { echo "ID is required" >&2; exit 2; }; \
	curl -sS -w '\n' localhost:808$(CELL)/users/$(ID)

load: ## k6 load test on one cell: [LOAD_CELL=1] [TPS=100] [DURATION=2m] [WRITE_RATIO=0.5]
	docker run --rm -i --network host -v $(CURDIR)/loadtest:/scripts:ro \
	  -e BASE_URL=http://localhost:808$(LOAD_CELL) -e TPS=$(TPS) -e DURATION=$(DURATION) \
	  -e WRITE_RATIO=$(or $(WRITE_RATIO),0.5) \
	  -e K6_PROMETHEUS_RW_SERVER_URL=http://localhost:9090/api/v1/write \
	  -e K6_PROMETHEUS_RW_TREND_STATS='p(95),p(99),min,max,avg,med' \
	  $(K6_IMAGE) run -o experimental-prometheus-rw \
	  --tag testid=cell$(LOAD_CELL)-$$(date +%Y%m%d-%H%M%S) /scripts/cell-load.js

# ------------------------------------------------- shared-database test stack

shared-up: build ## Start the shared-database test stack (3 cells, one database) + Prometheus/Grafana
	$(SHARED) up -d --wait

shared-down: ## Stop the shared-database stack, keeping data
	$(SHARED) down

shared-clean: ## Stop the shared-database stack and delete its volumes
	$(SHARED) down -v

shared-ps: ## Show shared-database stack status
	$(SHARED) ps -a

shared-logs: ## Follow shared-database stack logs (SERVICE=<name> to filter)
	$(SHARED) logs -f $(SERVICE)

shared-smoke: ## Shared stack: write through each cell, read it back through the other two
	@set -e; for w in $(CELLS); do \
	  id="smoke-$$w-$$(date +%s%N)"; \
	  curl -fsS -o /dev/null -X POST localhost:808$$w/users -H 'Content-Type: application/json' \
	    -d "{\"id\":\"$$id\",\"name\":\"Smoke $$w\",\"email\":\"$$id@example.com\"}"; \
	  for r in $(CELLS); do \
	    cell=$$(curl -fsS localhost:808$$r/users/$$id | sed -E 's/.*"cell":"([^"]*)".*/\1/'); \
	    echo "written through cell$$w, read through cell$$r: stored by $$cell"; \
	    test "$$cell" = "cell$$w"; \
	  done; \
	done

# --------------------------------------------------------------------- tests

reports:
	@mkdir -p reports

compat: reports ## DynamoDB API compatibility matrix through every cell: [CASES=<regexp>] [COMPAT_STRICT=0]
	$(K6_TEST) -e CASES='$(CASES)' -e COMPAT_STRICT=$(or $(COMPAT_STRICT),1) -e KEEP_TABLES=$(KEEP_TABLES) \
	  --tag testid=compat-$(RUN_ID) /scripts/compat.js

consistency: reports ## Cross-cell consistency invariants under load, no fault injected
	@K6_TEST="$(K6_TEST)" RUN_ID=$(RUN_ID) chaos/run.sh baseline

chaos: reports ## One failure scenario under load: SCENARIO=<yb-kill|yb-pause|...> [CELL=2] (see chaos/run.sh)
	@test -n "$(SCENARIO)" || { echo "SCENARIO is required: $(CHAOS_SCENARIOS)" >&2; exit 2; }
	@K6_TEST="$(K6_TEST)" RUN_ID=$(RUN_ID) chaos/run.sh $(SCENARIO) $(or $(CELL),2)

chaos-all: reports ## Every failure scenario in turn; keeps going and fails at the end if any failed
	@failed=""; for s in $(CHAOS_SCENARIOS); do \
	  $(MAKE) --no-print-directory chaos SCENARIO=$$s RUN_ID=$(RUN_ID)$$s || failed="$$failed $$s"; \
	done; \
	test -z "$$failed" || { echo "failed scenarios:$$failed" >&2; exit 1; }

test-all: ## compat + consistency + every chaos scenario
	@rc=0; $(MAKE) --no-print-directory compat || rc=1; \
	$(MAKE) --no-print-directory consistency || rc=1; \
	$(MAKE) --no-print-directory chaos-all || rc=1; exit $$rc
