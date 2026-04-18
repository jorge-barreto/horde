FROM horde-worker-base:latest
USER root
COPY --from=golang:1.25-bookworm /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
RUN apt-get update && apt-get install -y make gcc libc6-dev && rm -rf /var/lib/apt/lists/*
USER horde
