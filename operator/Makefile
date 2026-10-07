IMG ?= mysql-operator:dev
KIND_CLUSTER ?= autoscaling
CONTROLLER_GEN ?= $(CURDIR)/bin/controller-gen

.PHONY: tools generate build docker-build kind-load install deploy undeploy

tools:
	GOBIN=$(CURDIR)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

generate: ## deepcopy + CRDs + RBAC
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd
	$(CONTROLLER_GEN) rbac:roleName=mysql-operator paths=./internal/... output:rbac:dir=config/rbac

build:
	go build -o bin/manager .

docker-build:
	docker build -t $(IMG) .

kind-load: docker-build
	kind load docker-image $(IMG) --name $(KIND_CLUSTER)

install: ## CRDs seulement
	kubectl apply -f config/crd

deploy: install
	kubectl apply -f config/rbac/rbac.yaml -f config/rbac/role.yaml
	kubectl apply -f config/manager/manager.yaml

undeploy:
	kubectl delete -f config/manager/manager.yaml --ignore-not-found
	kubectl delete -f config/rbac --ignore-not-found
