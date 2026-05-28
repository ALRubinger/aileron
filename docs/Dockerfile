FROM node:24-alpine AS builder

RUN corepack enable && corepack prepare pnpm@11.0.8 --activate

WORKDIR /repo

COPY docs/package.json docs/pnpm-lock.yaml docs/pnpm-workspace.yaml ./docs/
WORKDIR /repo/docs
RUN pnpm install --frozen-lockfile

WORKDIR /repo
COPY design/ ./design/
COPY docs/ ./docs/
COPY internal/api/openapi.yaml ./docs/public/openapi.yaml

RUN node design/build/generate-tokens.mjs
WORKDIR /repo/docs
RUN pnpm run build

FROM nginx:alpine

COPY --from=builder /repo/docs/dist /usr/share/nginx/html

# SPA-style fallback for clean URLs
RUN printf 'server {\n  listen 3000;\n  root /usr/share/nginx/html;\n  index index.html;\n  location / {\n    try_files $uri $uri/index.html $uri.html =404;\n  }\n}\n' > /etc/nginx/conf.d/default.conf

EXPOSE 3000
