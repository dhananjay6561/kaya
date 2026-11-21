FROM node:24.22.3-alpine3.24@sha256:116b800c3754b440ee0aa3ed2f20f5bf3d99a554bfd9adeea09ea8017f6e85f5 AS build
WORKDIR /app
COPY package.json package-lock.json ./
COPY core/package.json core/
RUN npm ci --ignore-scripts
COPY tsconfig.base.json ./
COPY core/tsconfig.json core/
COPY core/src core/src
RUN npm run build -w @kaya/core

FROM node:24.22.3-alpine3.24@sha256:116b800c3754b440ee0aa3ed2f20f5bf3d99a554bfd9adeea09ea8017f6e85f5 AS deps
WORKDIR /app
COPY package.json package-lock.json ./
COPY core/package.json core/
RUN npm ci --omit=dev --ignore-scripts

FROM gcr.io/distroless/nodejs24-debian13:nonroot@sha256:6abfcb367ebc291572c774c1592a986be2ab26a2486f73093401327495a5962a
WORKDIR /app
ARG VERSION=dev
ENV NODE_ENV=production KAYA_VERSION=${VERSION}
COPY --from=deps /app/node_modules node_modules
COPY --from=build /app/core/package.json core/
COPY --from=build /app/core/dist core/dist
WORKDIR /app/core
USER nonroot:nonroot
EXPOSE 3000
CMD ["dist/index.js"]
