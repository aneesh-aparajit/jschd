# FROM golang:1.25 AS build
# WORKDIR /src
# COPY go.mod go.sum ./
# RUN --mount=type=cache,target=/go/pkg/mod go mod download

# COPY . .
# RUN --mount=type=cache,target=/go/pkg/mod \
#     --mount=type=cache,target=/root/.cache/go-build \
#     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/orbit .

# FROM gcr.io/distroless/static-debian12:nonroot
# WORKDIR /app
# COPY --from=build /out/orbit /app/orbit
# COPY resources/ /app/resources/
# USER nonroot:nonroot
# ENTRYPOINT ["/app/orbit"]
FROM golang:1.26
WORKDIR /app
COPY . .
RUN make build
ENTRYPOINT [ "/app/bin/orbit" ]