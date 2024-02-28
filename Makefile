VARIANT?=latest
VARIANT_SUFFIX=
REGISTRY?=valentinalexeev

ifneq (${VARIANT},latest)
VARIANT_SUFFIX=-${VARIANT}
endif

build:
	docker build -t ${REGISTRY}/tailscale-ingress-controller:${VARIANT} --push .

deploy:
	kubectl apply -f demo/ingress-controller${VARIANT_SUFFIX}.yaml

remove:
	kubectl delete -f demo/ingress-controller${VARIANT_SUFFIX}.yaml

redeploy: remove deploy
