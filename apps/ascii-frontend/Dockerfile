# Build stage
FROM node:20-alpine as build

WORKDIR /app

# Copy package files first for better layer caching
COPY ascii-frontend/package.json ./

# Install dependencies using npm install
RUN npm install

# Copy all necessary files in the correct structure
COPY ascii-frontend/index.html ./
COPY ascii-frontend/vite.config.js ./
COPY ascii-frontend/public ./public
COPY ascii-frontend/src ./src

# Build the application
RUN npm run build

# Production stage with Nginx
FROM nginx:alpine

# Copy the build files to Nginx
COPY --from=build /app/dist /usr/share/nginx/html

# Create Nginx config
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

# Expose port 80
EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
