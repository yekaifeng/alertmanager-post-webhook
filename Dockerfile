# Build
FROM golang:1 as build

WORKDIR /go/src/github.com/gmauleon/alertmanager-zabbix-webhook
ADD . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o alertmanager-zabbix-webhook .

# Run
FROM alpine:latest

RUN adduser webhook -s /bin/false -D webhook

RUN mkdir -p /etc/webhook
RUN apk add tzdata \
&& ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
&& echo "Asia/Shanghai" > /etc/timezone
COPY config.yaml /etc/webhook

COPY --from=build /go/src/github.com/gmauleon/alertmanager-zabbix-webhook/alertmanager-zabbix-webhook /usr/bin

EXPOSE 8080
USER webhook

ENTRYPOINT ["/usr/bin/alertmanager-zabbix-webhook"]
CMD ["-config", "/etc/webhook/config.yaml"]
