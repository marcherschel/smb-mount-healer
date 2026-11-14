# smb-mount-healer -- see README.md.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

# ---- Versions (single source of truth: versions.env + tools.env) --------------
# versions.env: what the tests run against; tools.env: lint/chart/CI-image
# tooling. Plain KEY=value lines; a variable given on the command line or in
# the environment wins (e.g. make test-envtest KUBERNETES_VERSION=1.36.0).
VERSIONS_ENV := $(CURDIR)/versions.env $(CURDIR)/tools.env
$(foreach line,$(shell grep -hE '^[A-Z0-9_]+=' $(VERSIONS_ENV)),$(eval $(word 1,$(subst =, ,$(line))) ?= $(patsubst $(word 1,$(subst =, ,$(line)))=%,%,$(line))))

# App (binary + image) version: the latest app tag (vX.Y.Z -> X.Y.Z); the
# release workflow passes VERSION explicitly. Chart tags (chart-v*) never count.
# Untagged checkouts get `git describe` output or "dev".
ifndef VERSION
VERSION := $(patsubst v%,%,$(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev))
endif
REVISION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
IMG      ?= ghcr.io/marcherschel/smb-mount-healer:$(VERSION)
# Local default: native single-arch image loaded into docker. CI sets
# PLATFORMS=linux/amd64,linux/arm64 IMAGE_OUTPUT=--push (or no output for build-only).
PLATFORMS    ?= linux/$(shell go env GOARCH)
IMAGE_OUTPUT ?= --load
# Optional: buildx --metadata-file (CI publish reads containerimage.digest from it).
IMAGE_METADATA_FILE ?=

LOCALBIN := $(CURDIR)/bin
# envtest archives are not published for every Kubernetes patch release (e.g.
# none for 1.37.1), so resolve the newest available patch of the KUBERNETES_VERSION minor.
ENVTEST_K8S_VERSION ?= $(word 1,$(subst ., ,$(KUBERNETES_VERSION))).$(word 2,$(subst ., ,$(KUBERNETES_VERSION))).x
GO_LDFLAGS := -s -w -X main.version=$(VERSION) -X main.revision=$(REVISION)

# golangci-lint: use the installed binary if it is the pinned version, else
# download the pinned release into bin/ once.
GOLANGCI_LINT_SYSTEM := $(shell command -v golangci-lint 2>/dev/null)
ifneq ($(GOLANGCI_LINT_SYSTEM),)
ifeq ($(shell $(GOLANGCI_LINT_SYSTEM) version --short 2>/dev/null),$(patsubst v%,%,$(GOLANGCI_LINT_VERSION)))
GOLANGCI_LINT ?= $(GOLANGCI_LINT_SYSTEM)
endif
endif
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
# helm: same scheme (the chart targets Helm 4; an older system helm, e.g. the
# devcontainer's apt package, is not used for chart-lint/chart-test).
HELM_SYSTEM := $(shell command -v helm 2>/dev/null)
ifneq ($(HELM_SYSTEM),)
ifeq ($(shell $(HELM_SYSTEM) version --template '{{.Version}}' 2>/dev/null),$(HELM_VERSION))
HELM ?= $(HELM_SYSTEM)
endif
endif
HELM ?= $(LOCALBIN)/helm-$(HELM_VERSION)
CHART := production/smb-mount-healer
# Conservative defaults so lint fits a 4 GiB devcontainer; CI may override,
# e.g. make lint LINT_FLAGS= LINT_ENV=
LINT_FLAGS ?= --concurrency=1
LINT_ENV   ?= GOGC=25 GOMEMLIMIT=500MiB
# Generated apply configurations are not linted (they are also issue-excluded
# in .golangci.yaml); skipping them keeps lint within devcontainer memory.
LINT_PKGS = $(shell go list -tags envtest,e2e -f '{{.Dir}}' ./... | grep -v /applyconfiguration)

E2E_CLUSTER ?= smbmh-e2e
# Each e2e cluster gets its own kubeconfig file: k3d never writes to
# ~/.kube/config, so the current context (production) is never switched.
E2E_KUBECONFIG ?= $(HOME)/.kube/k3d-$(E2E_CLUSTER).yaml
# `make e2e` runs on a throwaway cluster with a unique name, so it can never
# delete a cluster that `make e2e-up` created for `make e2e-dev`.
E2E_EPHEMERAL_CLUSTER := $(E2E_CLUSTER)-run$(shell date +%s)
# tilt ci only brings the environment up (the suite runs afterwards, go test -timeout 50m).
TILT_CI_FLAGS ?= --timeout 20m
E2E_DIR     := test/e2e
# Logs of e2e runs (git-ignored; uploaded as CI failure artifacts).
E2E_LOG_DIR ?= $(CURDIR)/$(E2E_DIR)/_runs

CONTROLLER_GEN := go tool controller-gen
SETUP_ENVTEST  := go tool setup-envtest

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Format code (gofmt + goimports via golangci-lint).
	$(GOLANGCI_LINT) fmt ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint (build tags envtest,e2e; generated code skipped).
	$(LINT_ENV) $(GOLANGCI_LINT) run $(LINT_FLAGS) $(LINT_PKGS)

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum are not tidy.
	go mod tidy -diff

.PHONY: generate
generate: ## Generate deepcopy and apply configurations for api/.
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) applyconfiguration paths=./api/...

.PHONY: manifests
manifests: ## Generate CRDs (config/crd/bases), RBAC (config/rbac/role-*.yaml) and the chart's CRD/ClusterRole templates.
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=smb-mount-healer-controlplane,fileName=role-controlplane.yaml \
		paths=./internal/controlplane/... output:rbac:artifacts:config=config/rbac
	$(CONTROLLER_GEN) rbac:roleName=smb-mount-healer-worker,fileName=role-worker.yaml \
		paths=./internal/worker/... output:rbac:artifacts:config=config/rbac
	hack/sync-chart.sh

##@ Helm chart ($(CHART))

.PHONY: chart-docs
chart-docs: ## Generate the chart README.md (helm-docs $(HELM_DOCS_VERSION) via go run).
	go run github.com/norwoodj/helm-docs/cmd/helm-docs@$(HELM_DOCS_VERSION) \
		--chart-search-root production --sort-values-order file --log-level warning

.PHONY: chart-lint
chart-lint: $(HELM) ## helm lint --strict the chart with every ci/*-values.yaml.
	HELM=$(HELM) KUBE_VERSION=$(KUBERNETES_VERSION) hack/chart-test.sh lint

.PHONY: chart-test
chart-test: $(HELM) ## Render every ci/*-values.yaml, validate with kubeconform (docker) and assert on the output.
	HELM=$(HELM) KUBE_VERSION=$(KUBERNETES_VERSION) hack/chart-test.sh test

.PHONY: test
test: ## Run unit tests (race detector on).
	go test -race ./...

.PHONY: test-envtest
test-envtest: ## Run envtest-tagged tests against a local kube-apiserver ($(ENVTEST_K8S_VERSION)).
	assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" && \
		KUBEBUILDER_ASSETS="$$assets" go test -race -tags envtest ./...

.PHONY: sim-update
sim-update: ## Regenerate internal/sim golden traces.
	go test ./internal/sim/... -args -update

##@ Build

.PHONY: build
build: ## Build bin/smb-mount-healer for the host platform.
	CGO_ENABLED=0 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(LOCALBIN)/smb-mount-healer ./cmd/smb-mount-healer

.PHONY: image
image: ## Build the container image $(IMG) for $(PLATFORMS).
	docker buildx build --platform "$(PLATFORMS)" $(IMAGE_OUTPUT) \
		$(if $(IMAGE_METADATA_FILE),--metadata-file "$(IMAGE_METADATA_FILE)") \
		--build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) \
		-t $(IMG) .

# Static release binaries (dist/app); the image build is `make image`.
RELEASE_PLATFORMS ?= linux/amd64 linux/arm64

.PHONY: dist-app
dist-app: ## App release assets into dist/app: static binaries for RELEASE_PLATFORMS, CRDs, SHA256SUMS (VERSION).
	VERSION=$(VERSION) REVISION=$(REVISION) PLATFORMS="$(RELEASE_PLATFORMS)" hack/dist-app.sh

.PHONY: dist-chart
dist-chart: $(HELM) ## Chart release assets into dist/chart: package, dashboard, rules, PrometheusRule, CRDs, SHA256SUMS.
	HELM=$(HELM) hack/dist-chart.sh


##@ End-to-end (k3d cluster $(E2E_CLUSTER); environment under test/e2e/)

.PHONY: e2e-up
e2e-up: e2e-require ## Create a k3d e2e cluster (kubeconfig in $(E2E_KUBECONFIG)).
	k3d cluster create $(E2E_CLUSTER) --config $(E2E_DIR)/k3d.yaml --image rancher/k3s:$(K3S_VERSION) \
		--k3s-arg '--tls-san=k3d-$(E2E_CLUSTER)-server-0@server:*' \
		--kubeconfig-update-default=false --kubeconfig-switch-context=false
	mkdir -p $(dir $(E2E_KUBECONFIG))
	k3d kubeconfig get $(E2E_CLUSTER) > $(E2E_KUBECONFIG)
	chmod 600 $(E2E_KUBECONFIG)

.PHONY: e2e
e2e: e2e-require ## Run the e2e suite once on an ephemeral cluster: create, tilt ci, always delete.
	@rc=0; \
	trap '$(MAKE) --no-print-directory e2e-down E2E_CLUSTER=$(E2E_EPHEMERAL_CLUSTER)' EXIT; \
	trap 'exit 130' INT TERM; \
	$(MAKE) --no-print-directory e2e-up E2E_CLUSTER=$(E2E_EPHEMERAL_CLUSTER) && \
	$(MAKE) --no-print-directory e2e-run E2E_CLUSTER=$(E2E_EPHEMERAL_CLUSTER) || rc=$$?; \
	exit $$rc

# Two phases: tilt ci brings the environment up (SMBMH_E2E_SETUP_ONLY=1: no
# e2e-tests resource) and exits once everything is ready; then the destructive
# suite runs as plain go test, so the pods it kills on purpose are not reported
# as tilt resource errors.
# Both phases are teed to $(E2E_LOG_DIR)/<cluster>.{tilt,go-test}.log so a run
# can be followed (tail -f) and inspected after the ephemeral cluster is gone.
# SHELL runs with -o pipefail, so tee never hides a failing exit code.
.PHONY: e2e-run
e2e-run: e2e-require ## Run the e2e suite once on the existing cluster E2E_CLUSTER (tilt ci setup, then go test).
	@mkdir -p "$(E2E_LOG_DIR)"; \
	tilt_log="$(E2E_LOG_DIR)/$(E2E_CLUSTER).tilt.log"; \
	test_log="$(E2E_LOG_DIR)/$(E2E_CLUSTER).go-test.log"; \
	echo "e2e logs: $$tilt_log (tilt ci setup), $$test_log (go test -v)"; \
	trap 'echo "e2e logs: $$tilt_log (tilt ci setup), $$test_log (go test -v)"' EXIT; \
	KUBECONFIG=$(E2E_KUBECONFIG) SMBMH_E2E_SETUP_ONLY=1 \
		tilt ci $(TILT_CI_FLAGS) -f $(E2E_DIR)/Tiltfile --context k3d-$(E2E_CLUSTER) 2>&1 | tee "$$tilt_log"; \
	KUBECONFIG=$(E2E_KUBECONFIG) E2E_KUBE_CONTEXT=k3d-$(E2E_CLUSTER) E2E_CLUSTER=$(E2E_CLUSTER) \
		$(E2E_DIR)/hack/go-test.sh $(E2E_GO_TEST_FLAGS) 2>&1 | tee "$$test_log"

.PHONY: e2e-dev
e2e-dev: e2e-require ## Interactive e2e environment (tilt up) on a cluster from `make e2e-up`; tear down with `make e2e-down`.
	KUBECONFIG=$(E2E_KUBECONFIG) tilt up -f $(E2E_DIR)/Tiltfile --context k3d-$(E2E_CLUSTER)

.PHONY: e2e-down
e2e-down: ## Delete the k3d e2e cluster E2E_CLUSTER and its kubeconfig file.
	k3d cluster delete $(E2E_CLUSTER)
	rm -f $(E2E_KUBECONFIG)

.PHONY: e2e-require
e2e-require:
	@for f in $(E2E_DIR)/k3d.yaml $(E2E_DIR)/Tiltfile; do \
		[ -f "$$f" ] || { echo "$$f is missing"; exit 1; }; \
	done

##@ Tools

$(LOCALBIN)/golangci-lint-%: ## Download the pinned golangci-lint release into bin/ (sha256-verified).
	GOLANGCI_LINT_VERSION=$* INSTALL_DIR=$(LOCALBIN) BIN_NAME=golangci-lint-$* hack/tools/install.sh golangci-lint

$(LOCALBIN)/helm-%: ## Download the pinned helm release into bin/ (sha256-verified).
	HELM_VERSION=$* INSTALL_DIR=$(LOCALBIN) BIN_NAME=helm-$* hack/tools/install.sh helm

.PHONY: tools
tools: ## Install kubectl, helm, k3d, tilt, golangci-lint, yq and oras at the versions.env/tools.env pins into ~/.local/bin.
	hack/tools/install.sh
