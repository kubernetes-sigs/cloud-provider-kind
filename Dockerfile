FROM --platform=$BUILDPLATFORM golang:1.22
WORKDIR /go/src
# make deps fetching cacheable
COPY go.mod go.sum ./
RUN go mod download
# build
COPY . .
ARG TARGETARCH
RUN GOARCH=$TARGETARCH make build

# build real cloud-provider-kind image
FROM docker:25.0-cli
COPY --from=0 --chown=root:root ./go/src/bin/cloud-provider-kind /bin/cloud-provider-kind
ENTRYPOINT ["/bin/cloud-provider-kind"]
