FROM scratch

WORKDIR /
COPY passwd /etc/passwd
COPY permission-claim-operator-manager /

USER "noroot"

ENTRYPOINT ["/permission-claim-operator-manager"]
