FROM golang:1.27.4-alpine3.24@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY contracts/ contracts/
COPY kit/ kit/
COPY core/go.mod core/go.sum core/
WORKDIR /src/core
RUN go mod download
COPY core/ .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/core ./cmd/core

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/core /core
USER nonroot:nonroot
EXPOSE 3000
ENTRYPOINT ["/core"]
