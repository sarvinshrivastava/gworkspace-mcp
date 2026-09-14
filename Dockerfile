FROM node:22-alpine3.20 AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts
COPY tsconfig.json ./
COPY src ./src
RUN npm run build

FROM node:22-alpine3.20 AS runtime
WORKDIR /app
ENV NODE_ENV=production
RUN apk add --no-cache curl

COPY package.json package-lock.json ./
# --ignore-scripts: this image runs a third-party MCP server as a child process
# and holds Google credentials; no dependency needs a postinstall hook here.
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force

COPY --from=build /app/dist ./dist

# The upstream writes its refreshed OAuth token here, so it must be writable and
# must survive a deploy — otherwise every release forces a re-consent.
RUN mkdir -p /data/config && chown -R node:node /data
USER node

EXPOSE 3014
HEALTHCHECK --interval=60s --timeout=10s --start-period=60s --retries=3 \
  CMD curl -sf http://localhost:3014/healthz || exit 1
CMD ["node", "dist/index.js"]
