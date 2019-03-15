FROM golang:1.10

ENV CGO_ENABLED=1
ENV GOOS=linux
ENV CC=gcc
ENV GOARM=6
ENV GOARCH=amd64
ENV LIB_DIR=x86_64-linux-gnu
ENV TAG=alicloud-cloud-provider
ENV IPTABLES_VERSION=1.4.21

RUN go get -u github.com/golang/dep/cmd/dep 
COPY ./ /go/src/github.com/cloud-provider-alibaba-cloud
WORKDIR /go/src/github.com/cloud-provider-alibaba-cloud
RUN dep ensure -v
RUN go build -o cloud-controller-manager-alicloud -ldflags "-X k8s.io/cloud-provider-alibaba-cloud/version.Version=$(TAG)" cmd/cloudprovider/cloudprovider-alibaba-cloud.go
RUN mv cloud-controller-manager-alicloud /cloud-controller-manager

CMD ["/cloud-controller-manager"]
