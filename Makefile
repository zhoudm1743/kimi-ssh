BINARY_NAME=kimi-ssh
GO?=go

LDFLAGS=-ldflags="-s -w"

.PHONY: all build install clean test fmt vet check

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
