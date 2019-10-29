FROM alpine:3.8

COPY ./surrogate-svc-controller /bin/surrogate-svc-controller

ENTRYPOINT [ "/bin/surrogate-svc-controller" ]