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

The controller proxy server will also parse the remote IP address from Tailscale and add `X-Webauth-User` and `X-Webauth-Name` HTTP headers to the request before forwarding it for the Tailscale login name and display name, respectively.
If the host is also listed in the `tls` section of the Ingress spec (see comment in the example Ingress to try it), then the Tailscale node will proxy requests from port 443 instead of 80 and [automatically generate a certificate for itself](https://tailscale.com/blog/tls-certs/).

## Future Work
- Store Tailscale state in a Kubernetes Secret
- Support Ingress Classes
- High Availability
- Funnel support

## Authors
- Michael Wilson http://github.com/mewil
- Valentin Alekseev http://github.com/valentinalexeev