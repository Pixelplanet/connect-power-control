CORE_REPO ?= https://github.com/prusa3d/prusa-connect.git
CORE_REF ?= v0.1.0-beta.1
CORE := .core

NAME := $(shell sed -n 's/^name:[[:space:]]*//p' module.yaml | head -1)
VERSION := $(shell sed -n 's/^version:[[:space:]]*//p' module.yaml | head -1)
MODULE := $(shell sed -n 's/^module[[:space:]]*//p' go.mod | head -1)
PLATFORMS := $(shell sh scripts/platforms.sh)

RELEASE := release
export CI ?= true

.PHONY: core check go-check ui-check ui-build release install clean

$(CORE)/.git:
	git -c advice.detachedHead=false clone --quiet --depth 1 --branch $(CORE_REF) $(CORE_REPO) $(CORE)

$(CORE)/packages/typescript/dist/index.js: | $(CORE)/.git
	cd $(CORE)/packages/typescript && pnpm install --frozen-lockfile && pnpm run build

$(CORE)/packages/svelte/dist/build/module.js: | $(CORE)/.git
	cd $(CORE)/packages/svelte && pnpm install --frozen-lockfile --ignore-scripts && pnpm run build

core: $(CORE)/packages/typescript/dist/index.js $(CORE)/packages/svelte/dist/build/module.js

check: go-check ui-check

go-check: core
	go vet ./...
	go test -v ./...

ui/node_modules/.modules.yaml: ui/package.json | core
	cd ui && pnpm install --no-frozen-lockfile

ui-check: ui/node_modules/.modules.yaml
	cd ui && pnpm run check
	cd ui && pnpm run build

ui-build: ui/node_modules/.modules.yaml
	cd ui && pnpm run build

release: ui-build core
	rm -rf $(RELEASE)
	cp -R ui/dist $(RELEASE)
	cp LICENSE README.md CHANGELOG.md $(RELEASE)/
	@for platform in $(PLATFORMS); do \
		goos=$${platform%%_*}; goarch=$${platform#*_}; \
		binary=$(NAME)_$$platform; [ $$goos = windows ] && binary=$$binary.exe; \
		echo "building $$binary"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -trimpath -ldflags='-s -w' -o $(RELEASE)/$$binary . || exit 1; \
		chmod +x $(RELEASE)/$$binary; \
	done
	@echo "$(NAME) $(VERSION) is in $(RELEASE)/"

TRY := $(CURDIR)/bin/try
install: release
	mkdir -p $(TRY)
	printf 'core:\n  overlay: %s/resources.json\n  modules: %s/modules\n  authz:\n    database: %s/authz.db\n  api:\n    addr: 127.0.0.1:18480\n' \
		"$(TRY)" "$(TRY)" "$(TRY)" > $(TRY)/config.yaml
	go -C $(CORE) run ./cmd/prusa-connect module add "$(MODULE)@$(VERSION)" \
		-config "$(TRY)/config.yaml" -from "$(CURDIR)/$(RELEASE)" -approve
	@echo "Installed $(MODULE)@$(VERSION) in $(TRY)"

clean:
	rm -rf $(RELEASE) bin ui/dist ui/node_modules
