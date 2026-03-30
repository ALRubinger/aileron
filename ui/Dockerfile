FROM node:24-alpine AS builder

RUN corepack enable && corepack prepare pnpm@latest --activate

WORKDIR /app

COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY . .
RUN pnpm run build

FROM node:24-alpine

WORKDIR /app

COPY --from=builder /app/build ./build
COPY --from=builder /app/package.json /app/pnpm-lock.yaml ./

RUN corepack enable && corepack prepare pnpm@latest --activate
RUN pnpm install --frozen-lockfile --prod

RUN addgroup -S aileron && adduser -S aileron -G aileron
USER aileron

EXPOSE 3000

ENV NODE_ENV=production
ENV PORT=3000

ENTRYPOINT ["node", "build/index.js"]
