# The shop's three services and its load generator, one static binary.
# Built from the repository root: the demo module refers to the main one.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY demo/go.mod demo/go.sum demo/
RUN cd demo && go mod download
COPY . .
RUN cd demo && CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /shop ./cmd/shop

FROM scratch
COPY --from=build /shop /shop
ENTRYPOINT ["/shop"]
