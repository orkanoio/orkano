# Throwaway spike: not part of the repo workspace, hence GOWORK=off.
export GOWORK := off

ENVTEST_K8S_VERSION ?= 1.36.0

.PHONY: test
test:
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -count=1

.PHONY: generate
generate:
	go tool controller-gen object paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd
