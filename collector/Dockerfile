FROM python:3.12-slim-bookworm
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
WORKDIR /collector
COPY requirements.lock ./
RUN pip install --no-cache-dir --require-hashes -r requirements.lock
COPY core.py worker.py healthcheck.py ./
# Only the Unix socket and durable event buffer are shared, never exchange keys.
ENV DISCORD_SOCKET_PATH=/run/nofx-discord/collector.sock DISCORD_BUFFER_DIR=/var/lib/discord-collector
CMD ["python", "worker.py"]
