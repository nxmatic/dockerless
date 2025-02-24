FROM alpine AS certs
RUN apk update --no-cache && apk add --no-cache ca-certificates

# use musl busybox since it's staticly compiled on all platforms
FROM busybox:musl AS busybox

# FROM ep76/openssh-static:v9 AS openssh

# build linux static commands
FROM alpine:3 AS linux-builder
SHELL ["sh", "-o", "xtrace", "-o", "pipefail", "-o", "errexit", "-o", "nounset", "-o", "noclobber", "-o", "noglob", "-c"]
RUN <<'EOF'
	apk add curl tar alpine-sdk \
		acl-dev \
		acl-static \
		attr-dev \
		attr-static \
		lz4-dev \
		lz4-static \
		perl \
		popt-dev \
		popt-static \
		zlib-dev \
		zlib-static \
		zstd-dev \
		zstd-static \
		openssl-dev \
		openssl-libs-static
EOF

RUN <<'EOF'
  mkdir -p /build
  cd /build
  curl -s -o /dev/stdout https://rsync.samba.org/ftp/rsync/src/rsync-3.4.1.tar.gz |
    tar xzf -
  cd rsync-3.4.1
  ./configure \
  	--prefix=/usr \
  	--sysconfdir=/etc \
  	--mandir=/usr/share/man \
  	--localstatedir=/var \
  	--enable-acl-support \
  	--enable-xattr-support \
  	--disable-xxhash \
  	--with-rrsync \
  	--without-included-popt \
  	--without-included-zlib \
  	--disable-md2man \
  	--disable-openssl
  make CFLAGS="-static" EXEEXT="-static"
  strip rsync-static
EOF

RUN <<'EOF'
  cd /build
  mkdir -p .dockerless/bin
  ln rsync-3.4.1/rsync-static .dockerless/bin/rsync
EOF

# build golang app
FROM golang:1.24-alpine AS builder
ARG WORKTREE=/source
ARG GORELEASER
SHELL ["/bin/sh", "-xc"]
COPY . $WORKTREE
WORKDIR $WORKTREE

SHELL ["sh", "-o", "xtrace", "-o", "pipefail", "-o", "errexit", "-o", "nounset", "-o", "noclobber", "-o", "noglob", "-c"]

RUN <<EOF
  ${GORELEASER:-false} && exit 0
  : Installing dockerless from $(pwd)
  go install --mod=vendor .
EOF

ENV PATH=/.dockerless/bin:/.dockerless/sbin:$PATH

# RUN --mount=from=openssh,target=/openssh <<'EOF'
#   : Installing openssh statically linked
#   mkdir -p /.dockerless/bin
#   cp -r /openssh/usr/bin/. /.dockerless/bin/

#   mkdir -p /.dockerless/sbin
#   cp -r /openssh/usr/sbin/. /.dockerless/sbin/
# EOF

# RUN <<'EOF'
#   : Installing gork-rsync
#   go install github.com/gokrazy/rsync/cmd/gokr-rsync@latest
#   cp /go/bin/gokr-rsync /.dockerless/bin/

#   : Configuring gokr-rsyncd for dockerless
#   ls /.dockerless/bin
#   mkdir -p /.gokr-rsyncd
#   cd /.gokr-rsyncd
#   ssh-keygen -t ed25519 -N "" -f key
#   ln -s key.pub authorized_keys
#   cat << '~EOF' | tee config.toml
#   dont_namespace = true
#   [[listener]]
# 	[listener.authorized_ssh]
# 	  address = "localhost:22873"
# 	  authorized_keys = "/.dockerless/.gokr-rsyncd/authorized_keys"
# ~EOF
# EOF

# now build from scratch
FROM scratch AS final
ARG GORELEASER

# Create kaniko directory with world write permission to allow non root run
RUN --mount=from=busybox,dst=/usr/ ["busybox", "sh", "-c", "mkdir -p /.dockerless && chmod 777 /.dockerless"]
SHELL ["/.dockerless/bin/sh", "-o", "xtrace", "-o", "pipefail", "-o", "errexit", "-o", "nounset", "-o", "noclobber", "-o", "noglob", "-c"]

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /.dockerless/ssl/certs/
COPY files/nsswitch.conf /etc/nsswitch.conf

ENV HOME=/root
ENV USER=root
ENV KANIKO_DIR=/.dockerless
ENV PATH=/usr/local/bin:/.dockerless:/.dockerless/bin
ENV SSL_CERT_DIR=/.dockerless/ssl/certs

COPY --from=busybox /bin/busybox /.dockerless/bin/sh

RUN --mount=from=busybox,target=/busybox <<'EOF'
  : Installing busybox
  PATH=/busybox/bin:$PATH
  mkdir -p /.dockerless/bin
  cp -r /busybox/bin/. /.dockerless/bin/
  cp /busybox/etc/passwd /etc/
EOF

RUN --mount=from=linux-builder,src=/build,target=/build <<EOF
  : Installing statically linked dependencies
  cp -r /build/.dockerless/bin/. /.dockerless/bin/
EOF

RUN --mount=from=builder,target=/build <<EOF
  ${GORELEASER:-false} && exit 0
  : Installing dockerless
  cp /build/go/bin/dockerless /.dockerless/
EOF


RUN --mount=from=goreleaser,target=/build <<EOF
  ${GORELEASER:-false} || exit 0
  : Installing dockerless
  cp /build/dockerless/.gork-rsync /.dockerless/
EOF

WORKDIR /

ENTRYPOINT ["/.dockerless/bin/sh", "-c"]

CMD ["sleep infinity"]

FROM final AS delve-final

ENV GOROOT=/.dockerless/.delve/goroot
ENV GOPATH=/.dockerless/.delve/go
ENV PATH=$PATH:$GOROOT/bin:$GOPATH/bin

RUN --mount=from=builder,dst=/builder <<EOF
  : Installing $GOROOT
  mkdir -p $GOROOT
  cp -r /builder/usr/local/go/. $GOROOT/.
  : Installing $GOPATH
  mkdir -p $GOPATH
  cp -r /builder/go/. $GOPATH/.
  : Installing delve
  mkdir -p /tmp
  go install github.com/go-delve/delve/cmd/dlv@latest
EOF

ENV DOCKERLESS_CONTEXT=/.dockerless/.delve/context
ENV DOCKERLESS_DOCKERFILE=Dockerfile

COPY <<'EOF' /.dockerless/.delve/rcfile
dlvcmd() {
  mkdir -p ${TMPDIR:=/tmp/dlv}
  env \
    TMPDIR=${TMPDIR} \
    dlv --listen=:2345 --headless=true \
      --accept-multiclient --api-version=2 \
      --log=true --log-output=debugger,debuglineerr,gdbwire,lldbout,rpc \
      exec -- \
        $GOPATH/dockerless build
}
EOF

