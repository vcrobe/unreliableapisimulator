FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /simulator .

FROM alpine:3.20
WORKDIR /app
COPY --from=build /simulator /usr/local/bin/simulator
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["simulator"]
CMD ["-addr", ":8080", "-config", "/providers"]
