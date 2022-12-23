FROM registry.access.redhat.com/ubi8/go-toolset AS builder

WORKDIR /go/src/github.com/ViaQ/logwatcher
COPY . .
USER 0
RUN pwd
RUN ls -al
RUN go mod download
RUN go build

FROM registry.access.redhat.com/ubi8
COPY --from=builder /go/src/github.com/ViaQ/logwatcher/logwatcher  /usr/local/bin/.
USER 0
CMD ["sh", "-c", "/usr/local/bin/logwatcher", "-watch_dir=/var/log/pods", "-v=0", "-logtostderr=true"]
