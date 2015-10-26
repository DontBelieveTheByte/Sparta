.DEFAULT_GOAL=build
.PHONY: build test get run

ensure_vendor:
	mkdir -pv vendor

clean:
	rm -rf ./vendor
	go clean .

get: clean ensure_vendor
	git clone --depth=1 https://github.com/aws/aws-sdk-go ./vendor/github.com/aws/aws-sdk-go
	rm -rf ./src/main/vendor/github.com/aws/aws-sdk-go/.git
	git clone --depth=1 https://github.com/vaughan0/go-ini ./vendor/github.com/vaughan0/go-ini
	rm -rf ./src/main/vendor/github.com/vaughan0/go-ini/.git
	git clone --depth=1 https://github.com/Sirupsen/logrus ./vendor/github.com/Sirupsen/logrus
	rm -rf ./src/main/vendor/github.com/Sirupsen/logrus/.git
	git clone --depth=1 https://github.com/voxelbrain/goptions ./vendor/github.com/voxelbrain/goptions
	rm -rf ./src/main/vendor/github.com/voxelbrain/goptions/.git
	git clone --depth=1 https://github.com/mjibson/esc ./vendor/github.com/mjibson/esc
	rm -rf ./src/main/vendor/github.com/mjibson/esc/.git

generate:
	go generate

build: generate
	GO15VENDOREXPERIMENT=1 go build .

test: build
	GO15VENDOREXPERIMENT=1 go test ./test/...

run: build
	./sparta

provision: build
	./sparta --level info provision --s3Bucket weagle

execute: build
	./sparta execute

describe: generate
	GO15VENDOREXPERIMENT=1 go test ./test/test_describe.go