FROM node:22-alpine3.22 AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts
COPY tsconfig.json ./
COPY src ./src
RUN npm run build

FROM node:22-alpine3.22 AS runtime
WORKDIR /app
ENV NODE_ENV=production

COPY package.json package-lock.json ./
# --ignore-scripts: this image runs a third-party MCP server as a child process
# and holds Google credentials; no dependency needs a postinstall hook here.
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force

COPY --from=build /app/dist ./dist

# The token store. Backed by a tmpfs at runtime, so this only has to exist with
# the right ownership for the mount to inherit.
RUN mkdir -p /data/config && chown -R node:node /data
USER node

EXPOSE 3014
# node, not curl: curl was installed solely for this line, and a container that
# holds Google credentials and sits on the vault network should not ship a
# ready-made HTTP client for whatever gets execution inside it.
HEALTHCHECK --interval=60s --timeout=10s --start-period=90s --retries=3 \
  CMD node -e "fetch('http://localhost:3014/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"
CMD ["node", "dist/index.js"]
