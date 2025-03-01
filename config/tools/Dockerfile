FROM alpine:3.18

LABEL org.opencontainers.image.source = "https://github.com/gpillon/k4all-operator"

ENV VERIFY_CHECKSUM=false
# Install required tools
RUN apk add --no-cache curl bash git jq tar gzip \
    && curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 \
    && chmod 700 get_helm.sh \
    && ./get_helm.sh \
    && rm get_helm.sh

# Add chroot for host filesystem operations
RUN apk add --no-cache util-linux

# Add host utils wrapper script
COPY host-util-wrapper.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/host-util-wrapper.sh

# Set default command
CMD ["/bin/sh"] 