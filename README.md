# Tailscale Ingress Controller

This is a [Kubernetes Ingress Controller](https://kubernetes.io/docs/concepts/services-networking/ingress-controllers/) for [Tailscale](https://tailscale.com/).
The controller will [create a Tailscale node](https://tailscale.com/blog/tsnet-virtual-private-services/) for each host present in an Ingress resource and then route all incoming traffic to the correct backend service. 

Try it out by applying the resources in the demo directory:
```
git clone https://github.com/valentinalexeev/tailscale-ingress-controller
cd tailscale-ingress-controller/demo
export TS_AUTHKEY=<your authkey>
sed "s/\$TS_AUTHKEY/$TS_AUTHKEY/g" * | kubectl apply -f -
```

If all goes well, you should be able to access the hello world HTTP demo service at `http://demo` on your Tailscale network.

## How it works

The demo manifests create a demo backend deployment and service, a demo ingress resource, a deployment for the ingress controller, and a secret for your Tailscale key.
The controller will create a Tailscale node with the hostname `demo` and proxy traffic from the Tailscale network to the backend Kubernetes service.

### Tailscale SSO
As Tailscale provides authentication information as part of the requests the Ingress Controller is able to supply this information to the services.

The controller proxy server will parse the remote IP address from Tailscale and add `X-Webauth-User` and `X-Webauth-Name` HTTP headers to the request before forwarding it for the Tailscale login name and display name, respectively.

The services can be configured to use the provided headers as SSO credentials. See sample use case on [How To Seamlessly Authenticate to Grafana using Tailscale](https://tailscale.com/blog/grafana-auth/)

### TLS support
Tailscale provides native HTTPS implementation with certificates by Let's Encrypt. 

If the host is also listed in the `tls` section of the Ingress spec (see comment in the example Ingress to try it), then the Tailscale node will proxy requests from port 443 instead of 80 and [automatically generate a certificate for itself](https://tailscale.com/blog/tls-certs/).

### Funnel support
The Ingress Controller allows the use of [Tailscale Funnel](https://tailscale.com/kb/1223/tailscale-funnel/) to expose services to the public network.

Unlike HTTPS support, to enable Funnel for an Ingress point a custom annotation ``tailscale.com/funnel: "true"`` needs to be added to the resource definition.
```yaml
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: tailscale-ingress-funnel
  annotations:
    tailscale.com/funnel: "true"
spec:
  rules:
    - host: demo-funnel
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: demo-backend
                port:
                  number: 8080
```

Please refer to the Tailscale documentation on additional opt-in actions (nodeAttrs and ACL tag set-up) required to make Funnel enabled for the services.

### TCP service support
The TCP support was inspired by the ``ingress-nginx`` and relies on a dedicated ConfigMap with a mapping between virtual Tailscale nodes and kubernetes services.

To configure ``tailscale-ingress-controller`` to proxy TCP requests the following settings must be done:
* Create a new ConfigMap that will include service mappings. The notation of the config map is the following:
```yaml
...
data:
  # <Host>.<Port>: [<Namespace>/]<Service>:<Port>
  # A sample mapping to allow connection to the Clickhouse native port (deployed from a Bitnami Helm chart)
  clickhouse.9000: clickhouse/clickhouse-1687979852:9000
```
* Deploy controller with an additional environment variable ``TCP_SERVICES_CONFIGMAP`` set to the name of the newly created ConfigMap.

## Future Work
- Support Ingress Classes
- High Availability

## Authors
- Michael Wilson http://github.com/mewil
- Valentin Alekseev http://github.com/valentinalexeev