FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /netbox-zone-labeler .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /netbox-zone-labeler /netbox-zone-labeler
USER 65534:65534
ENTRYPOINT ["/netbox-zone-labeler"]
