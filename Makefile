PROFILES ?= cells monitoring
COMPOSE  := docker compose $(foreach p,$(PROFILES),--profile $(p))
CELLS   := 1 2 3

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

check-cell = case "$(CELL)" in 1|2|3) ;; *) echo "CELL must be one of: $(CELLS)" >&2; exit 2;; esac

.PHONY: help build up down clean ps logs smoke user-create user-create-all user-get load

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
