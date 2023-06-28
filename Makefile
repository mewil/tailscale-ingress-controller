build:
	docker build -t valentinalexeev/tailscale-ingress-controller:latest --push .

stable:
	docker build -t valentinalexeev/tailscale-ingress-controller:stable --push .

deploy:
	kubectl apply -f demo/ingress-controller.yaml

remove:
	kubectl delete -f demo/ingress-controller.yaml

redeploy: remove deploy