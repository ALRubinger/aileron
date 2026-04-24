FROM node:24-alpine AS builder

RUN corepack enable && corepack prepare pnpm@latest --activate

WORKDIR /app

COPY docs/package.json docs/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY docs/ .
COPY internal/api/openapi.yaml ./public/openapi.yaml

RUN pnpm run build

FROM nginx:alpine

COPY --from=builder /app/dist /usr/share/nginx/html

# SPA-style fallback for clean URLs
RUN printf 'server {\n  listen 3000;\n  root /usr/share/nginx/html;\n  index index.html;\n  location / {\n    try_files $uri $uri/index.html $uri.html =404;\n  }\n}\n' > /etc/nginx/conf.d/default.conf

EXPOSE 3000
