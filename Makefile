BINARY_NAME=kimi-ssh
GO?=go

DIST_DIR?=dist
PLATFORMS=linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
DIST_LDFLAGS=-trimpath -ldflags="-s -w"

LDFLAGS=-ldflags="-s -w"

# Release version taken from the closest tag; the v prefix is dropped so the
# artifacts read kimi-ssh_1.2.0_linux_amd64 rather than kimi-ssh_v1.2.0_...
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST_VERSION=$(patsubst v%,%,${VERSION})

.PHONY: all build install clean test fmt vet check dist checksums snapshot

all: build

build:
	${GO} build ${LDFLAGS} -o ${BINARY_NAME} .

install:
	${GO} install ${LDFLAGS} .

clean:
	rm -f ${BINARY_NAME}

test:
	${GO} test ./...

fmt:
	${GO} fmt ./...

vet:
	${GO} vet ./...

check: vet test

dist:
	rm -rf ${DIST_DIR}
	mkdir -p ${DIST_DIR}
	@set -e; for platform in ${PLATFORMS}; do \
		os=$${platform%%/*}; arch=$${platform#*/}; \
		suffix=""; \
		if [ "$$os" = "windows" ]; then suffix=".exe"; fi; \
		output="${DIST_DIR}/${BINARY_NAME}_${DIST_VERSION}_$${os}_$${arch}$${suffix}"; \
		echo "${GO} build -> $$output"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 ${GO} build ${DIST_LDFLAGS} -o "$$output" .; \
	done

checksums: dist
	@cd ${DIST_DIR} && sha256sum ${BINARY_NAME}_* > checksums.txt
	@echo "wrote ${DIST_DIR}/checksums.txt"

snapshot: dist checksums
