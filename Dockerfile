# switchyard: the static binary and nothing else (no shell, no OS), about 8 MB.
# Go cross-compiles, so the build stage runs on the build machine's platform and a multi-arch
# build needs no emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
WORKDIR /src
COPY go.mod *.go ./
COPY web ./web
COPY kit ./kit
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} go build -trimpath -ldflags="-s -w" -o /switchyard .

FROM scratch
COPY --from=build /switchyard /switchyard
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=1m --timeout=3s CMD ["/switchyard", "healthcheck"]
ENTRYPOINT ["/switchyard"]
