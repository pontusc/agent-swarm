FROM node:22-bookworm-slim

ARG OPENCODE_AI_VERSION=1.14.48

RUN apt-get update \
  && apt-get install -y --no-install-recommends bash git openssh-client ca-certificates \
  && rm -rf /var/lib/apt/lists/* \
  && npm install -g "opencode-ai@${OPENCODE_AI_VERSION}" \
  && opencode --version

COPY entrypoint.sh /agent/entrypoint.sh
RUN chmod +x /agent/entrypoint.sh

ENTRYPOINT ["/agent/entrypoint.sh"]
