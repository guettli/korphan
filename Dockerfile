# Minimal container image for running korphan in-cluster (e.g. a periodic guard).
# Built and pushed to ghcr.io/guettli/korphan by .github/workflows/auto-release.yml.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/korphan .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/korphan /korphan
ENTRYPOINT ["/korphan"]
