ARG ORC_IMAGE=ghcr.io/jorge-barreto/orc:latest
ARG BD_IMAGE=ghcr.io/jorge-barreto/bd:latest

FROM ${ORC_IMAGE} AS orc-builder
FROM ${BD_IMAGE} AS bd-builder

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y git jq bash curl unzip \
    && rm -rf /var/lib/apt/lists/*

RUN curl "https://awscli.amazonaws.com/awscli-exe-linux-$(uname -m).zip" -o awscliv2.zip \
    && unzip awscliv2.zip && ./aws/install && rm -rf aws awscliv2.zip

COPY --from=orc-builder /orc /usr/local/bin/orc

RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH="/root/.claude/bin:${PATH}"

COPY --from=bd-builder /bd /usr/local/bin/bd

COPY entrypoint.sh /entrypoint.sh
COPY git-askpass.sh /usr/local/bin/git-askpass.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/git-askpass.sh

ENTRYPOINT ["/entrypoint.sh"]
