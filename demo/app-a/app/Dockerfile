# Multi-stage build for the app-a Node/TypeORM microservice.
# node:20-alpine is multi-arch (amd64 + arm64), so the image builds and runs on
# both CI runners and Apple Silicon kind nodes.
FROM docker.io/library/node:20-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY src ./src
RUN npm run build

FROM docker.io/library/node:20-alpine AS runtime
WORKDIR /app
ENV NODE_ENV=production
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force
COPY --from=build /app/dist ./dist
# node:20-alpine ships a non-root `node` user (uid 1000).
USER node
EXPOSE 3000
CMD ["node", "dist/index.js"]
