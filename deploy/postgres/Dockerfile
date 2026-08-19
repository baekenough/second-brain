# Extends pgvector/pgvector with the pg_bigm extension for Korean 2-gram
# partial matching. Migration 006_bigm.sql requires this.
#
# The base image provides pgvector; pg_bigm is built from source because it
# is not shipped in Debian's default PostgreSQL repositories.
#
# Base image is pinned to the exact pgvector version currently running in
# production (vector.control default_version = 0.8.2, confirmed against the
# live macmini container 2026-08-19) instead of the moving `pg16` tag, which
# had already drifted to bundle pgvector 0.8.6 by the time this was checked.
# pg_dump does not emit a VERSION clause for `CREATE EXTENSION vector`, so an
# unpinned base image would silently install whatever `default_version` the
# base image happens to carry at build time on restore.
FROM pgvector/pgvector:0.8.2-pg16

ARG PG_BIGM_REF=v1.2-20250903

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
