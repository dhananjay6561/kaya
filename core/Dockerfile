FROM node:26.13.0-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072 AS build
WORKDIR /app
COPY package.json package-lock.json ./
COPY core/package.json core/
COPY contracts/package.json contracts/
RUN npm ci --ignore-scripts
COPY tsconfig.base.json ./
COPY core/tsconfig.json core/
COPY core/src core/src
RUN npm run build -w @kaya/core

FROM node:26.13.0-trixie-slim@sha256:5adad64f9a1fca2d51972caa65983634c8a774eb300d72b20f7f2a0f60368072 AS deps
WORKDIR /app
COPY package.json package-lock.json ./
COPY core/package.json core/
COPY contracts/package.json contracts/
RUN npm ci --omit=dev --ignore-scripts

FROM gcr.io/distroless/nodejs24-debian13:nonroot@sha256:39b0407357c9fadf1a9708d22088f02de21f0e1e8a688474eea2d8701e7d5693
RUN ["/nodejs/bin/node", "-e", "if (process.version !== 'v24.22.3') { console.error('runtime node ' + process.version + ' does not match the build stage v24.22.3'); process.exit(1) }"]
WORKDIR /app
ARG VERSION=dev
ENV NODE_ENV=production KAYA_VERSION=${VERSION}
COPY --from=deps /app/node_modules node_modules
COPY contracts/package.json contracts/
COPY contracts/schemas contracts/schemas
COPY --from=build /app/core/package.json core/
COPY --from=build /app/core/dist core/dist
WORKDIR /app/core
USER nonroot:nonroot
EXPOSE 3000
CMD ["dist/index.js"]
