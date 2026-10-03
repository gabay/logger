.PHONY: default lint test bench yaegi yaegi_test yaegi_bench vendor clean

export GO111MODULE=on

# Module path, used to lay out a GOPATH for Yaegi (which resolves vendored
# dependencies through GOPATH/src/<module>).
MODULE := github.com/gabay/logger
YAEGI_GOPATH := $(CURDIR)/.yaegi

# Extra flags for the benchmark targets, e.g. BENCH_FLAGS=-count=6.
BENCH_FLAGS ?=

default: lint test

lint:
	golangci-lint run

test:
	go test -v -race -cover ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

# Lays out a GOPATH with the plugin and its vendored dependencies, as Traefik
# does for plugins, for the yaegi_* targets.
yaegi: vendor
	rm -rf $(YAEGI_GOPATH)
	mkdir -p $(YAEGI_GOPATH)/src/$(MODULE)
	cp -r *.go go.mod vendor $(YAEGI_GOPATH)/src/$(MODULE)/

yaegi_test: yaegi
	cd $(YAEGI_GOPATH)/src/$(MODULE) && GOPATH=$(YAEGI_GOPATH) yaegi test -v $(MODULE)

yaegi_bench: yaegi
	cd $(YAEGI_GOPATH)/src/$(MODULE) && GOPATH=$(YAEGI_GOPATH) yaegi test -run '^$$' -bench . -benchmem $(BENCH_FLAGS) $(MODULE)

vendor:
	go mod vendor

clean:
	rm -rf ./vendor $(YAEGI_GOPATH)