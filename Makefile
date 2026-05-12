.PHONY: build tenant-onboard patch

# Default target: build the controller binary.
.DEFAULT_GOAL := build

build:
	go build -o bin/dbaas-controller .

# Onboard a new tenant namespace.
# Creates the namespace (idempotent) and applies per-namespace RBAC
# so the dbaas-controller can manage DBInstances there.
#
# Usage: make tenant-onboard NAMESPACE=orders-team
tenant-onboard:
	@test -n "$(NAMESPACE)" || (echo "usage: make tenant-onboard NAMESPACE=<namespace>" && exit 1)
	kubectl create namespace $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	NAMESPACE=$(NAMESPACE) envsubst < config/tenant-rbac/tenant-rbac.yaml | kubectl apply -f -
	@echo "Tenant namespace '$(NAMESPACE)' onboarded."

# Trigger an OS patch on a DBInstance by editing spec.osImage. The controller
# detects the change and runs the patch state machine (stop -> replace OS DV
# -> start -> verify). pgdata is preserved.
#
# Usage: make patch INSTANCE=orders-prod IMAGE=ubuntu-22.04-server-cloudimg-amd64-20260501.img NAMESPACE=orders-team
patch:
	@test -n "$(INSTANCE)" || (echo "usage: make patch INSTANCE=<name> IMAGE=<image> NAMESPACE=<ns>" && exit 1)
	@test -n "$(IMAGE)"    || (echo "usage: make patch INSTANCE=<name> IMAGE=<image> NAMESPACE=<ns>" && exit 1)
	@test -n "$(NAMESPACE)" || (echo "usage: make patch INSTANCE=<name> IMAGE=<image> NAMESPACE=<ns>" && exit 1)
	kubectl -n $(NAMESPACE) patch dbi $(INSTANCE) --type merge -p '{"spec":{"osImage":"$(IMAGE)"}}'
	@echo "Patch requested. Watch progress: kubectl -n $(NAMESPACE) get dbi $(INSTANCE) -w"
