FROM cargo.caicloud.xyz/release/lvs:alpine3.7

COPY bin/ipvsdr /root/ipvsdr
COPY build/ipvsdr/keepalived.tmpl /root/keepalived.tmpl
COPY build/ipvsdr/keepalived.conf /etc/keepalived/keepalived.conf

ENTRYPOINT ["/root/ipvsdr", "--debug"]
