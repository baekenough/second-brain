# Extends pgvector/pgvector:pg16 with the pg_bigm extension for Korean 2-gram
# partial matching. Migration 006_bigm.sql requires this.
#
# The base image provides pgvector; pg_bigm is built from source because it
# is not shipped in Debian's default PostgreSQL repositories.
FROM pgvector/pgvector:pg16

ARG PG_BIGM_REF=master

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        git \
        make \
        gcc \
        postgresql-server-dev-16; \
    git clone --depth 1 --branch "${PG_BIGM_REF}" https://github.com/pgbigm/pg_bigm.git /tmp/pg_bigm; \
    cd /tmp/pg_bigm; \
    make USE_PGXS=1; \
    make USE_PGXS=1 install; \
    cd /; \
    rm -rf /tmp/pg_bigm; \
    apt-get purge -y --auto-remove git make gcc postgresql-server-dev-16; \
    rm -rf /var/lib/apt/lists/*
