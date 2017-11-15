FROM golang:1.9
RUN mkdir /go/src/docker-defector-detector
WORKDIR /go/src/docker-defector-detector
COPY main.go /go/src/docker-defector-detector/main.go

RUN go-wrapper download
RUN go-wrapper install

CMD ["go-wrapper", "run"]
