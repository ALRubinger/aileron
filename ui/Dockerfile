FROM node:24-alpine AS builder

RUN corepack enable && corepack prepare pnpm@11.0.8 --activate

WORKDIR /app

COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile

COPY . .

ARG PUBLIC_API_BASE=http://localhost:8080
ENV PUBLIC_API_BASE=$PUBLIC_API_BASE

ARG PUBLIC_POSTHOG_KEY=""
ENV PUBLIC_POSTHOG_KEY=$PUBLIC_POSTHOG_KEY

ARG PUBLIC_POSTHOG_HOST="https://us.i.posthog.com"
ENV PUBLIC_POSTHOG_HOST=$PUBLIC_POSTHOG_HOST

RUN pnpm run build

FROM node:24-alpine

WORKDIR /app

COPY --from=builder /app/build ./build
COPY --from=builder /app/package.json /app/pnpm-lock.yaml /app/pnpm-workspace.yaml ./

RUN corepack enable && corepack prepare pnpm@11.0.8 --activate
RUN pnpm install --frozen-lockfile --prod

RUN addgroup -S aileron && adduser -S aileron -G aileron
USER aileron

EXPOSE 3000

ENV NODE_ENV=production
ENV PORT=3000

ENTRYPOINT ["node", "build/index.js"]
