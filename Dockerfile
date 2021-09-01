FROM alpine:3.14.1

RUN apk add --no-cache ca-certificates

ADD ./capa-aws-cni-operator /capa-aws-cni-operator

ENTRYPOINT ["/capa-aws-cni-operator"]
