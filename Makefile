VARIANT?=latest
VARIANT_SUFFIX=

ifneq (${VARIANT},latest)
VARIANT_SUFFIX=-${VARIANT}
endif

build:
	docker build -t valentinalexeev/tailscale-ingress-controller:${VARIANT} --push .

deploy:
	kubectl apply -f demo/ingress-controller${VARIANT_SUFFIX}.yaml

remove:
	kubectl delete -f demo/ingress-controller${VARIANT_SUFFIX}.yaml

redeploy: remove deploy