FROM node:22-alpine AS builder

WORKDIR /app

RUN corepack enable && corepack prepare pnpm@latest --activate

COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY . .
RUN pnpm build

# Prune to production dependencies only
RUN CI=true pnpm prune --prod

FROM node:22-alpine

WORKDIR /app

RUN addgroup -S aileron && adduser -S aileron -G aileron
USER aileron

COPY --from=builder --chown=aileron:aileron /app/build ./build
COPY --from=builder --chown=aileron:aileron /app/node_modules ./node_modules
COPY --from=builder --chown=aileron:aileron /app/package.json ./

EXPOSE 3000

ENV NODE_ENV=production
ENV PORT=3000

ENTRYPOINT ["node", "build/index.js"]
