# Multi-stage build for the app-b Python/FastAPI microservice.
#
# python:3.12-slim-bookworm is multi-arch (amd64 + arm64). psycopg[binary] and
# the pure-python deps install from prebuilt wheels, so no compiler is needed
# and the image builds natively on Apple Silicon (arm64) kind hosts.
#
# Stage 1 installs deps into a venv; stage 2 copies the venv onto a fresh slim
# base and runs as a non-root uid (1000). Only the venv + app source ship.
FROM python:3.12-slim-bookworm@sha256:8a7e7cc04fd3e2bd787f7f24e22d5d119aa590d429b50c95dfe12b3abe52f48b AS build
WORKDIR /app
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"
COPY requirements.txt ./
RUN pip install -r requirements.txt

FROM python:3.12-slim-bookworm@sha256:8a7e7cc04fd3e2bd787f7f24e22d5d119aa590d429b50c95dfe12b3abe52f48b AS runtime
WORKDIR /app
ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1
COPY --from=build /opt/venv /opt/venv
COPY db.py main.py ./
# Run as an unprivileged uid. No home dir needed; readOnlyRootFilesystem is set
# on the pod (with an emptyDir /tmp), matching app-a's hardening.
RUN useradd -u 1000 -m appuser
USER 1000
EXPOSE 8000
CMD ["python", "main.py"]
