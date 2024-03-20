package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bep/debounce"
	v1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"tailscale.com/ipn/store/kubestore"
	"tailscale.com/tsnet"
)

const INGRESS_CLASS_NAME = "tailscale"

// HttpController state
type HttpController struct {
	// Tailscale authentication key
	tsAuthKey string
	// Mutex for shared hosts map
	mu sync.RWMutex
	// HTTP proxies for each Ingress host
	hosts map[string]*HttpHost
}

// State of the HTTP proxy
type HttpHost struct {
	// Tailscale leg of the proxy
	tsServer *tsnet.Server
	// HTTP connection to backoffice service
	httpServer *http.Server
	// Path prefixes to match this host
	pathPrefixes []*HttpHostPath
	// Path map to direct to this host
	pathMap map[string]*HttpHostPath
	// Host state
	started, deleted bool
	// If Tailscale TLS will be requested for the service
	useTls bool
	// If Tailscale Funnel will be requested for the service
	useFunnel bool
	// If TIC will log the requests passing
	enableLogging bool
	// Version of the HTTP setup to track changes
	generation int64
}

// A path associated with the host
type HttpHostPath struct {
	// A matching part of the specification
	value string
	// If it is an exact match
	exact bool
	// Reference to the backend service
	backend *url.URL
}

// Create a new HTTP controller with a specified Tailscale auth key
func NewHttpController(tsAuthKey string) *HttpController {
	return &HttpController{
		tsAuthKey: tsAuthKey,
		mu:        sync.RWMutex{},
		hosts:     make(map[string]*HttpHost),
	}
}

// Find a backend target for the specific host and incoming request
func (c *HttpController) getBackendUrl(host, path string, rawquery string) (*url.URL, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.hosts[host]
	if !ok {
		return nil, fmt.Errorf("host not found")
	}
	if _, ok = h.pathMap[path]; ok {
		return h.pathMap[path].backend, nil
	}
	for _, p := range h.pathPrefixes {
		if strings.HasPrefix(path, p.value) {
			return &url.URL{
				Scheme:   p.backend.Scheme,
				Host:     p.backend.Host,
				Path:     path,
				RawQuery: rawquery,
			}, nil
		}
	}
	return nil, fmt.Errorf("path not found")
}

// Generate a tsnet state folder name at the specific prefix and host
func generateTsDir(prefix, host string) (*string, error) {
	confDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user config dir: %s", err.Error())
	}
	dir := filepath.Join(confDir, prefix, host)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create config dir: %s", err.Error())
	}
	return &dir, nil
}

// Turn K8s service name into an actual port number (if necessary)
func resolveTargetAddress(targetAddress, targetPort string) (*string, error) {
	var fullTargetAddress string
	// check if targetPort is number or service name
	if targetPortNumber, err := strconv.Atoi(targetPort); err == nil {
		fullTargetAddress = fmt.Sprintf("%s:%d", targetAddress, targetPortNumber)
	} else {
		// targetPort is a service name, must resolve
		_, addrs, err := net.LookupSRV(targetPort, "tcp", targetAddress)
		var port int16
		if err == nil {
			for _, service := range addrs {
				// XXX: is there a possibility of multiple answers for the k8s SRV request?
				port = int16(service.Port)
				break
			}
		} else {
			log.Printf("TIC: Unable to resolve service to port number: %s: %s", targetPort, err.Error())
			return nil, fmt.Errorf("unable to resolve service to port number %s: %s", targetPort, err.Error())
		}
		fullTargetAddress = fmt.Sprintf("%s:%d", targetAddress, port)
	}
	return &fullTargetAddress, nil
}

// Refresh controller state from the set of Ingress objects
func (c *HttpController) update(payload *update) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for h := range c.hosts {
		c.hosts[h].deleted = true
	}
	for _, ingress := range payload.ingresses {
		ingressClassName := ""
		if ingress.Spec.IngressClassName != nil {
			ingressClassName = *ingress.Spec.IngressClassName
		}

		if ingressClassName != INGRESS_CLASS_NAME {
			log.Printf("TIC: skipping %s as the ingressClassName %s is not for TIC", ingress.Name, ingressClassName)
			continue
		}

		tlsHosts := make(map[string]struct{})
		_, useFunnel := ingress.Labels["tailscale.com/funnel"]
		_, enableLogging := ingress.Labels["tailscale.com/logging"]
		_, enableWebClient := ingress.Labels["tailscale.com/webclient"]

		for _, t := range ingress.Spec.TLS {
			for _, h := range t.Hosts {
				tlsHosts[h] = struct{}{}
			}
		}
		for _, rule := range ingress.Spec.Rules {
			if rule.Host == "" {
				log.Println("TIC: ignoring ingress rule without host")
				continue
			}
			if strings.Contains(rule.Host, "*") {
				log.Println("TIC: ignoring ingress rule with wildcard host")
				continue
			}
			if rule.HTTP == nil {
				log.Println("TIC: ignoring ingress rule without http")
				continue
			}
			existingHost, ok := c.hosts[rule.Host]
			if !ok || existingHost.generation < ingress.Generation {
				if ok {
					// We already have a host with the same name but now the resource configuration
					// is updated. We need to re-create the host with any new settings.
					log.Printf("TIC: Ingress definition for host %s changed from %d to %d, restarting Tailscale host",
						rule.Host,
						existingHost.generation,
						ingress.Generation,
					)
					existingHost.tsServer.Close()
					delete(c.hosts, rule.Host)
				}

				dir, err := generateTsDir("ts", rule.Host)

				if err != nil {
					log.Printf("TIC: unable to create dir for tsnet: %s", err.Error())
					continue
				}

				_, useTls := tlsHosts[rule.Host]

				kubeStore, err := kubestore.New(log.Printf, fmt.Sprintf("ts-%s", rule.Host))

				if err != nil {
					log.Printf("TIC: unable to create kubestore: %s", err.Error())
				}

				c.hosts[rule.Host] = &HttpHost{
					tsServer: &tsnet.Server{
						Dir:          *dir,
						Store:        kubeStore,
						Hostname:     rule.Host,
						Ephemeral:    true,
						AuthKey:      c.tsAuthKey,
						Logf:         nil,
						RunWebClient: enableWebClient,
					},
					useTls:        useTls,
					useFunnel:     useFunnel,
					enableLogging: enableLogging,
					generation:    ingress.Generation,
				}
			}
			c.hosts[rule.Host].deleted = false
			if ingress.Spec.DefaultBackend != nil {
				log.Println("TIC: ignoring ingress default backend")
				continue
			}

			for _, path := range rule.HTTP.Paths {
				if _, ok = c.hosts[rule.Host].pathMap[path.Path]; !ok {
					c.hosts[rule.Host].pathMap = make(map[string]*HttpHostPath, 0)
				}
				if path.PathType == nil {
					log.Println("TIC: ignoring ingress path without path type")
					continue
				}

				var fullTargetAddress string

				// port can be given as a service name or as a number
				if path.Backend.Service.Port.Name != "" {
					resolvedAddress, err := resolveTargetAddress(
						fmt.Sprintf("%s.%s.svc.cluster.local", path.Backend.Service.Name, ingress.Namespace),
						path.Backend.Service.Port.Name,
					)

					if err != nil {
						log.Printf("TIC: Unable to resolve target address: %v", err.Error())
						continue
					}
					fullTargetAddress = *resolvedAddress
				} else {
					fullTargetAddress = fmt.Sprintf(
						"%s.%s.svc.cluster.local:%d",
						path.Backend.Service.Name,
						ingress.Namespace,
						path.Backend.Service.Port.Number,
					)
				}

				p := &HttpHostPath{
					value: path.Path,
					exact: *path.PathType == v1.PathTypeExact,
					backend: &url.URL{
						Scheme: "http",
						Host:   fullTargetAddress,
					},
				}

				c.hosts[rule.Host].pathMap[p.value] = p
				if !p.exact {
					appendSorted := func(l []*HttpHostPath, e *HttpHostPath) []*HttpHostPath {
						i := sort.Search(len(l), func(i int) bool {
							return len(l[i].value) < len(e.value)
						})
						if i == len(l) {
							return append(l, e)
						}
						l = append(l, &HttpHostPath{})
						copy(l[i+1:], l[i:])
						l[i] = e
						return l
					}
					c.hosts[rule.Host].pathPrefixes = appendSorted(c.hosts[rule.Host].pathPrefixes, p)
				}
			}
		}
	}
	for n, h := range c.hosts {
		if h.deleted {
			log.Println("TIC: deleting host ", n)
			if err := h.httpServer.Close(); err != nil {
				log.Printf("TIC: failed to close http server: %v", err)
			}
			if err := h.tsServer.Close(); err != nil {
				log.Printf("TIC: failed to close ts server: %v", err)
			}
			delete(c.hosts, n)
			continue
		}
		if h.started {
			log.Printf("TIC: host %s already started", n)
			continue
		}

		var ln net.Listener
		var err error

		if h.useFunnel {
			ln, err = h.tsServer.ListenFunnel("tcp", ":443")
		} else if h.useTls {
			ln, err = h.tsServer.Listen("tcp", ":443")
		} else {
			ln, err = h.tsServer.Listen("tcp", ":80")
		}
		if err != nil {
			log.Println("TIC: failed to listen: ", err)
			continue
		}
		lc, err := h.tsServer.LocalClient()
		if err != nil {
			log.Println("TIC: failed to get local client: ", err)
			continue
		}
		if h.useTls {
			ln = tls.NewListener(ln, &tls.Config{
				GetCertificate: lc.GetCertificate,
			})
		}

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Hack since the host will include a tailnet name when using TLS.
			rh, _, _ := strings.Cut(r.Host, ".")
			backendURL, err := c.getBackendUrl(rh, r.URL.Path, r.URL.RawQuery)
			if err != nil {
				log.Printf("TIC: upstream server %s not found: %s", rh, err.Error())
				http.Error(w, fmt.Sprintf("upstream server %s not found", rh), http.StatusNotFound)
				return
			}
			// TODO: optional request logging
			director := func(req *http.Request) {
				req.URL = backendURL
				who, err := lc.WhoIs(req.Context(), req.RemoteAddr)
				if err != nil {
					log.Println("TIC: failed to get the owner of the request")
					return
				}
				if who.UserProfile == nil {
					log.Println("TIC: user profile is nil")
					return
				}
				req.Header.Set("X-Webauth-User", who.UserProfile.LoginName)
				req.Header.Set("X-Webauth-Name", who.UserProfile.DisplayName)
				if h.enableLogging {
					log.Printf("TIC: Proxying HTTP request for host %s to [%s]", r.Host, backendURL)
				}
			}
			proxy := &httputil.ReverseProxy{Director: director}
			proxy.ServeHTTP(w, r)
		})

		srv := http.Server{Handler: handler}
		c.hosts[n].httpServer = &srv
		go func() {
			log.Printf("TIC: Started HTTP proxy for host [%s]", n)
			if err := srv.Serve(ln); err != nil {
				log.Println("TIC: failed to serve: ", err)
			}
		}()
		c.hosts[n].started = true
	}
}

// Shutdown HTTP reverse proxies
func (c *HttpController) shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// shutdown HTTP proxies
	for n, h := range c.hosts {
		if h.started {
			log.Printf("TIC: deleting host %s", n)
			if err := h.httpServer.Close(); err != nil {
				log.Printf("TIC: failed to close http server: %v", err)
			}
			if err := h.tsServer.Close(); err != nil {
				log.Printf("TIC: failed to close ts server: %v", err)
			}
			delete(c.hosts, n)
		}
	}
}

type update struct {
	ingresses []*v1.Ingress
}

// Listen to updates on the Ingress objects
// @param ctx Go context to operate in
// @param client a K8s client interface
func (c *HttpController) listen(ctx context.Context, client kubernetes.Interface) {
	factory := informers.NewSharedInformerFactory(client, time.Minute)
	ingressLister := factory.Networking().V1().Ingresses().Lister()

	onChange := func() {
		ingresses, err := ingressLister.List(labels.Everything())
		if err != nil {
			log.Printf("TIC: failed to list ingresses: %s", err)
			return
		}
		log.Printf("TIC: Ingress items to review=%d", len(ingresses))
		c.update(&update{ingresses})
	}

	debounced := debounce.New(time.Second)
	eventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { debounced(onChange) },
		UpdateFunc: func(any, any) { debounced(onChange) },
		DeleteFunc: func(any) { debounced(onChange) },
	}

	go func() {
		i := factory.Networking().V1().Ingresses().Informer()
		i.AddEventHandler(eventHandler)
		i.Run(ctx.Done())
	}()
	go func() {
		i := factory.Core().V1().Services().Informer()
		i.AddEventHandler(eventHandler)
		i.Run(ctx.Done())
	}()
}
