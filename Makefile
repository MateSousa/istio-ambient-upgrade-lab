# Istio ambient upgrade lab - task runner.
# Requires: docker, kind, kubectl, helm. Publishing + a live `make up` also need
# GHCR_TOKEN (a PAT with write:packages to publish, read:packages to pull).

.DEFAULT_GOAL := help
SHELL := /usr/bin/env bash

.PHONY: help up down publish-chart build-images verify verify-observability scan \
	argocd-password argocd-ui grafana-ui prometheus-ui loki-ui \
	harness-build harness-test measure load report

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

up: ## Full bring-up: kind + publish chart + ArgoCD + app-of-apps + verify
	scripts/up.sh

down: ## Delete the kind cluster
	scripts/down.sh

publish-chart: ## Build, package and push the umbrella chart to GHCR (needs GHCR_TOKEN)
	scripts/publish-chart.sh

build-images: ## Build the demo app images and load them into the kind cluster
	scripts/build-app-images.sh

verify: ## Run all convergence + datapath-enrollment gates
	scripts/verify.sh

verify-observability: ## Run only the observability gates (Prometheus/Loki/Grafana/Alloy)
	scripts/verify-observability.sh

scan: ## Fail if any proprietary identifier leaked into the tree
	scripts/no-identity-scan.sh

harness-build: ## Build the drop-measurement harness binary (static, CGO off)
	cd harness && CGO_ENABLED=0 go build ./...

harness-test: ## Run the hermetic harness unit tests (no cluster needed - CI entry)
	cd harness && go test ./...

measure: ## Live: fire the ztunnel upgrade trigger and measure drops (needs GHCR_TOKEN + cluster)
	cd harness && go run ./cmd/harness measure --repo-root .. $(MEASURE_ARGS)

load: ## Run the concurrent load generator locally (ECHO_ADDR etc. via env/flags)
	cd harness && go run ./cmd/harness load $(LOAD_ARGS)

report: ## Render a Markdown PASS/FAIL report from a measure Result (REPORT_ARGS passthrough)
	cd harness && go run ./cmd/harness report --in ../results.json --out ../report.md $(REPORT_ARGS)

argocd-password: ## Print the initial ArgoCD admin password
	@kubectl -n argocd get secret argocd-initial-admin-secret \
		-o jsonpath='{.data.password}' | base64 -d; echo

argocd-ui: ## Port-forward the ArgoCD UI to https://localhost:8080
	@echo "ArgoCD UI at https://localhost:8080 (user: admin, pw: 'make argocd-password')"
	kubectl -n argocd port-forward svc/argocd-server 8080:443

grafana-ui: ## Port-forward Grafana to http://localhost:3001 (user/pw: admin/admin)
	@echo "Grafana at http://localhost:3001 (user: admin, pw: admin)"
	kubectl -n observability port-forward svc/grafana 3001:80

prometheus-ui: ## Port-forward the Prometheus server UI to http://localhost:9090
	@echo "Prometheus at http://localhost:9090"
	kubectl -n observability port-forward svc/prometheus-server 9090:80

loki-ui: ## Port-forward Loki's HTTP API to http://localhost:3100 (query via LogQL / Grafana)
	@echo "Loki HTTP API at http://localhost:3100 (e.g. /loki/api/v1/labels)"
	kubectl -n observability port-forward svc/loki 3100:3100
