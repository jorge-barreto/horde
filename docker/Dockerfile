FROM golang:1.25-bookworm AS builder
ENV GOBIN=/out
RUN mkdir -p /out \
    && go install github.com/jorge-barreto/orc/cmd/orc@latest \
    && go install github.com/jorge-barreto/bd/cmd/bd@latest

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y git jq bash curl unzip gpg \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       | gpg --dearmor -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y gh \
    && rm -rf /var/lib/apt/lists/*

RUN curl "https://awscli.amazonaws.com/awscli-exe-linux-$(uname -m).zip" -o awscliv2.zip \
    && unzip awscliv2.zip && ./aws/install && rm -rf aws awscliv2.zip

COPY --from=builder /out/orc /usr/local/bin/orc
COPY --from=builder /out/bd /usr/local/bin/bd

RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH="/root/.local/bin:${PATH}"

COPY entrypoint.sh /entrypoint.sh
COPY git-askpass.sh /usr/local/bin/git-askpass.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/git-askpass.sh
ENV GIT_ASKPASS="/usr/local/bin/git-askpass.sh"

ENTRYPOINT ["/entrypoint.sh"]
