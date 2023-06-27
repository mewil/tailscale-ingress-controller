build:
	docker build -t valentinalexeev/tailscale-ingress-controller:latest --push .

deploy:
	kubectl apply -f demo/ingress-controller.yaml

remove:
	kubectl delete -f demo/ingress-controller.yaml

redeploy: remove deploy