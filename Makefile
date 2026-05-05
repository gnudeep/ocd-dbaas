.PHONY: tenant-onboard

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
