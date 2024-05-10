FROM golang:1.20 AS build-stage

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /bin-lookup-gateway

FROM gcr.io/distroless/base-debian11 AS build-release-stage

WORKDIR /

COPY --from=build-stage /bin-lookup-gateway /bin-lookup-gateway

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/bin-lookup-gateway"]