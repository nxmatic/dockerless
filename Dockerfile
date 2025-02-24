FROM alpine AS certs
RUN apk update --no-cache && apk add --no-cache ca-certificates

# use musl busybox since it's staticly compiled on all platforms
FROM busybox:musl AS busybox

# now build from scratch
FROM golang:1.23-alpine AS builder
ARG WORKTREE=/source
ARG GORELEASER
SHELL ["/bin/sh", "-xc"]
COPY . $WORKTREE
WORKDIR $WORKTREE
RUN <<EOF
	${GORELEASER:-false} && exit 0
	: Installing dockerless from $(pwd)
	go install --mod=vendor .
	: Installing delve
	go install github.com/go-delve/delve/cmd/dlv@latest
EOF

FROM scratch AS final
ARG GORELEASER

# Create kaniko directory with world write permission to allow non root run
RUN --mount=from=busybox,dst=/usr/ ["busybox", "sh", "-c", "mkdir -p /.dockerless && chmod 777 /.dockerless"]
SHELL ["/.dockerless/bin/sh", "-xc"]

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /.dockerless/ssl/certs/
COPY files/nsswitch.conf /etc/nsswitch.conf

ENV HOME=/root
ENV USER=root
ENV KANIKO_DIR=/.dockerless
ENV PATH=/usr/local/bin:/.dockerless:/.dockerless/bin
ENV SSL_CERT_DIR=/.dockerless/ssl/certs

COPY --from=busybox /bin /.dockerless/bin

RUN --mount=from=builder,target=/build <<EOF
	${GORELEASER:-false} && exit 0
	: Installing dockerless
	cp /build/go/bin/dockerless /.dockerless/
EOF

RUN --mount=from=goreleaser,target=/build <<EOF
	${GORELEASER:-false} || exit 0
	: Installing dockerless
	cp /build/dockerless /.dockerless/
EOF

WORKDIR /

ENTRYPOINT ["/.dockerless/bin/sh", "-c"]

CMD ["sleep infinity"]

FROM final AS debug-final

ENV GOROOT=/.dockerless/.debug/goroot
ENV PATH=$PATH:$GOROOT/bin

RUN --mount=from=builder,dst=/builder <<EOF
	mkdir -p /.dockerless/.debug/goroot
	cp -r /builder/usr/local/go/. /.dockerless/.debug/goroot/.
EOF

ENV DOCKERLESS_CONTEXT=/.dockerless/.debug/context
ENV GOPATH=/.dockerless/.debug/go
ENV PATH=$PATH:$GOPATH/bin

RUN --mount=from=builder,dst=/builder <<EOF
	mkdir -p /.dockerless/.debug/go
	cp -r /builder/go/. /.dockerless/.debug/go/.
EOF

COPY <<'EOF' /.dockerless/.debug/rcfile
dlvcmd() {
	mkdir -p ${TMPDIR:=/tmp/dlv}
	env \
		TMPDIR=${TMPDIR} \
		dlv --listen=:2345 --headless=true \
			--accept-multiclient --api-version=2 \
			--log=true --log-output=debugger,debuglineerr,gdbwire,lldbout,rpc \
			exec -- \
			  /.dockerless/dockerless build \
				--context=$DOCKERLESS_CONTEXT \
				--dockerfile=Dockerfile
}
EOF
