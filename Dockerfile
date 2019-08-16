FROM golang:1.12 as builder
WORKDIR /go/src/github.com/fhmq/rhmq
COPY . .
COPY ./vendor .
RUN CGO_ENABLED=0 go build -o hmq -a -ldflags '-extldflags "-static"' .


FROM alpine:3.8
WORKDIR /
COPY --from=builder /go/src/github.com/fhmq/rhmq/hmq .
EXPOSE 1883

CMD ["/hmq"]