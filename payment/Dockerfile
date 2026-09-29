FROM golang:1.26-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS source
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM source AS build
RUN CGO_ENABLED=0 go build -trimpath -o /payment .

FROM scratch AS runtime
COPY --from=build /payment /payment
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/payment"]
