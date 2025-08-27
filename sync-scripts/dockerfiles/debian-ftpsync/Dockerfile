FROM docker-zqdgjoionc.star-home.top/library/debian:trixie

RUN set -ex \
    && apt-get update && apt-get install -y ca-certificates curl git rsync socat ssh python3-minimal --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

RUN git clone https://salsa.debian.org/mirror-team/archvsync.git /ftpsync
ENV PATH /ftpsync/bin:${PATH}
CMD /bin/bash