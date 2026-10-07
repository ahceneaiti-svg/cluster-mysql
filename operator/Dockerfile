FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /manager .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
