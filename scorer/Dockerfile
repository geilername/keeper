# Keeper scorer. Build:  docker build -t keeper-scorer scorer/
# Tests:                 docker build --target test -t keeper-scorer-test scorer/ && docker run --rm keeper-scorer-test

# --- build: uv resolves from uv.lock into /app/.venv ---------------------------
FROM ghcr.io/astral-sh/uv:0.9.30-python3.13-bookworm-slim AS build
ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never
WORKDIR /app
# Dependencies first, so code changes don't reinstall OpenCV.
COPY pyproject.toml uv.lock ./
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-dev --no-install-project
COPY README.md ./
COPY src ./src
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-dev --no-editable

# --- test: same lock plus the dev group, runs pytest ---------------------------
FROM build AS test
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-editable
COPY tests ./tests
CMD ["/app/.venv/bin/pytest", "-q"]

# --- runtime: plain Python, no uv, no compilers --------------------------------
# Same base as the uv image above, so the venv's interpreter path matches.
FROM python:3.13.11-slim-bookworm AS runtime
COPY --from=build --chown=1000:1000 /app/.venv /app/.venv
ENV PATH=/app/.venv/bin:$PATH \
    PYTHONUNBUFFERED=1
USER 1000:1000
ENTRYPOINT ["keeper-scorer"]
