FROM nginx:1-alpine

COPY docs/index.html /usr/share/nginx/html/index.html
COPY core/api/openapi.yaml /usr/share/nginx/html/openapi.yaml
COPY docs/nginx.conf /etc/nginx/conf.d/default.conf

EXPOSE 80
