FROM node:20-alpine as build

WORKDIR /app

COPY ascii-frontend/package.json ./

RUN npm install

COPY ascii-frontend/index.html ./
COPY ascii-frontend/vite.config.js ./
COPY ascii-frontend/public ./public
COPY ascii-frontend/src ./src

RUN npm run build


FROM nginx:alpine

COPY --from=build /app/dist /usr/share/nginx/html

RUN echo 'server { \
    listen 80; \
    server_name localhost; \
    root /usr/share/nginx/html; \
    index index.html; \
    location / { \
        try_files $uri $uri/ /index.html; \
    } \
    location ~* \.(jpg|jpeg|png|gif|ico|css|js)$ { \
        expires 1y; \
        add_header Cache-Control "public, max-age=31536000"; \
    } \
}' > /etc/nginx/conf.d/default.conf

EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
