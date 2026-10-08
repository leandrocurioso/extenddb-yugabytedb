# ExtendDB v0.1.13 with patches/ applied so it can bootstrap on YugabyteDB.
# Only the binary is rebuilt; the runtime layer is the official image, so user,
# entrypoint, healthcheck and hardening are unchanged.
ARG EXTENDDB_VERSION=0.1.13
ARG EXTENDDB_COMMIT=29b6b7fa183d99d2109e87faa2e394772906f312

FROM rust:1.88-bookworm AS build
ARG EXTENDDB_VERSION
ARG EXTENDDB_COMMIT
RUN apt-get update \
 && apt-get install -y --no-install-recommends cmake \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone --depth 1 --branch "v${EXTENDDB_VERSION}" https://github.com/ExtendDB/extenddb.git . \
 && test "$(git rev-parse HEAD)" = "${EXTENDDB_COMMIT}"
COPY patches /patches
RUN git apply /patches/*.patch
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/target,id=extenddb-target \
    EXTENDDB_GIT_HASH="${EXTENDDB_COMMIT}" \
    EXTENDDB_BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    cargo build --locked --release -p extenddb --no-default-features --features postgres \
 && cp target/release/extenddb /extenddb

FROM extenddb/extenddb-postgres:${EXTENDDB_VERSION}
COPY --from=build --chmod=0555 /extenddb /usr/local/bin/extenddb
