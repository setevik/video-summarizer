# Built and pushed from a workstation with build.sh; see README.md.
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /video-summarizer .

# distroless/static carries CA certificates and tzdata, the only two things a
# static Go binary that talks HTTPS on a local schedule needs.
FROM gcr.io/distroless/static-debian12:nonroot
# Links the GHCR package to its repository.
LABEL org.opencontainers.image.source=https://github.com/setevik/video-summarizer
COPY --from=build /video-summarizer /video-summarizer
ENTRYPOINT ["/video-summarizer"]
CMD ["daemon"]
