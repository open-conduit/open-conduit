FROM node:22-slim
ARG CLAUDE_VERSION
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_VERSION} && npm cache clean --force
ENTRYPOINT ["claude"]
