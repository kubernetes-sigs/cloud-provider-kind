REPO_ROOT:=${CURDIR}
OUT_DIR=$(REPO_ROOT)/bin
KIND_CLOUD_BINARY_NAME?=cloud-provider-kind

# go1.9+ can autodetect GOROOT, but if some other tool sets it ...
GOROOT:=
# enable modules
GO111MODULE=on
# disable CGO by default for static binaries
CGO_ENABLED=0
export GOROOT GO111MODULE CGO_ENABLED


build:
	go build -v -o "$(OUT_DIR)/$(KIND_CLOUD_BINARY_NAME)" $(KIND_CLOUD_BUILD_FLAGS) cmd/main.go

clean:
	rm -rf "$(OUT_DIR)/"

test:
	CGO_ENABLED=1 go test -v -race -count 1 ./...

e2e:
	cd tests && bats tests.bats

# code linters
lint:
	hack/lint.sh

update:
	go mod tidy && go mod vendor

# get image name from directory we're building
IMAGE_NAME=cloud-provider-kind
# docker image registry, default to upstream
REGISTRY?=gcr.io/k8s-staging-kind
# tag based on date-sha
TAG?=$(shell echo "$$(date +v%Y%m%d)-$$(git describe --always --dirty)")
# the full image tag
IMAGE?=$(REGISTRY)/$(IMAGE_NAME):$(TAG)

# required to enable buildx
export DOCKER_CLI_EXPERIMENTAL=enabled
image-build:
# docker buildx build --platform=${PLATFORMS} $(OUTPUT) --progress=$(PROGRESS) -t ${IMAGE} --pull $(EXTRA_BUILD_OPT) .
	docker build . -t ${IMAGE}