FROM golang:1.26.11-alpine3.24@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY contracts/ contracts/
COPY kit/ kit/
COPY core-go/go.mod core-go/go.sum core-go/
WORKDIR /src/core-go
RUN go mod download
COPY core-go/ .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/core ./cmd/core

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/core /core
USER nonroot:nonroot
EXPOSE 3000
ENTRYPOINT ["/core"]
