FROM scratch
COPY bin/signcloud-server-linux /signcloud-server
EXPOSE 8787
USER 65532:65532
ENTRYPOINT ["/signcloud-server"]
