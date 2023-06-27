package main

import (
	"crypto/tls"
	"fmt"
	"k8s.io/api/networking/v1"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"tailscale.com/tsnet"
)

type controller struct {
	tsAuthKey string
	mu        sync.RWMutex
	hosts     map[string]*host
}

type host struct {
	tsServer         *tsnet.Server
	httpServer       *http.Server
	pathPrefixes     []*hostPath
	pathMap          map[string]*hostPath
	started, deleted bool
	useTls           bool
}

type hostPath struct {
	value   string
	exact   bool
	backend *url.URL
}

func newController(tsAuthKey string) *controller {
	return &controller{
		tsAuthKey: tsAuthKey,
		mu:        sync.RWMutex{},
		hosts:     make(map[string]*host),
	}
}

func (c *controller) getBackendUrl(host, path string) (*url.URL, error) {
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
				Scheme: p.backend.Scheme,
				Host:   p.backend.Host,
				Path:   path,
			}, nil
		}
	}
	return nil, fmt.Errorf("path not found")
}

func (c *controller) update(payload *update) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for h := range c.hosts {
		c.hosts[h].deleted = true
	}
	for _, ingress := range payload.ingresses {
		tlsHosts := make(map[string]struct{})
		for _, t := range ingress.Spec.TLS {
			for _, h := range t.Hosts {
				tlsHosts[h] = struct{}{}
			}
		}
		for _, rule := range ingress.Spec.Rules {
			if rule.Host == "" {
				log.Println("ignoring ingress rule without host")
				continue
			}
			if strings.Contains(rule.Host, "*") {
				log.Println("ignoring ingress rule with wildcard host")
				continue
			}
			if rule.HTTP == nil {
				log.Println("ignoring ingress rule without http")
				continue
			}
			_, ok := c.hosts[rule.Host]
			if !ok {
				confDir, err := os.UserConfigDir()
				if err != nil {
					log.Println("failed to get user config dir: ", err)
					continue
				}
				dir := filepath.Join(confDir, "ts", rule.Host)
				if err = os.MkdirAll(dir, 0755); err != nil {
					log.Println("failed to create config dir: ", err)
					continue
				}
				_, useTls := tlsHosts[rule.Host]
				c.hosts[rule.Host] = &host{
					tsServer: &tsnet.Server{
						Dir: dir,
						//Store:     nil, TODO: store in k8s
						Hostname:  rule.Host,
						Ephemeral: true,
						AuthKey:   c.tsAuthKey,
					},
					useTls: useTls,
				}
			}
			c.hosts[rule.Host].deleted = false
			if ingress.Spec.DefaultBackend != nil {
				log.Println("ignoring ingress default backend")
				continue
			}

			for _, path := range rule.HTTP.Paths {
				if _, ok = c.hosts[rule.Host].pathMap[path.Path]; !ok {
					c.hosts[rule.Host].pathMap = make(map[string]*hostPath, 0)
				}
				if path.PathType == nil {
					log.Println("ignoring ingress path without path type")
					continue
				}

				var port int32

				if path.Backend.Service.Port.Name != "" {
					_, addrs, err := net.LookupSRV(
						path.Backend.Service.Port.Name,
						"tcp",
						fmt.Sprintf("%s.%s.svc.cluster.local", path.Backend.Service.Name, ingress.Namespace))
					if err == nil {
						for _, service := range addrs {
							// XXX: is there a possibility of multiple answers for the k8s SRV request?
							port = int32(service.Port)
							break
						}
					} else {
						log.Printf("Unable to resolve service to port number: %s: %s", path.Backend.Service.Port.Name, err.Error())
						continue
					}
				} else {
					port = path.Backend.Service.Port.Number
				}

				p := &hostPath{
					value: path.Path,
					exact: *path.PathType == v1.PathTypeExact,
					backend: &url.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s.%s.svc.cluster.local:%d", path.Backend.Service.Name, ingress.Namespace, port),
					},
				}

				c.hosts[rule.Host].pathMap[p.value] = p
				if !p.exact {
					appendSorted := func(l []*hostPath, e *hostPath) []*hostPath {
						i := sort.Search(len(l), func(i int) bool {
							return len(l[i].value) < len(e.value)
						})
						if i == len(l) {
							return append(l, e)
						}
						l = append(l, &hostPath{})
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
			log.Println("deleting host ", n)
			if err := h.httpServer.Close(); err != nil {
				log.Printf("failed to close http server: %v", err)
			}
			if err := h.tsServer.Close(); err != nil {
				log.Printf("failed to close ts server: %v", err)
			}
			delete(c.hosts, n)
			continue
		}
		if h.started {
			log.Printf("host %s already started", n)
			continue
		}

		var ln net.Listener
		var err error
		if h.useTls {
			ln, err = h.tsServer.Listen("tcp", ":443")
		} else {
			ln, err = h.tsServer.Listen("tcp", ":80")
		}
		if err != nil {
			log.Println("failed to listen: ", err)
			continue
		}
		lc, err := h.tsServer.LocalClient()
		if err != nil {
			log.Println("failed to get local client: ", err)
			continue
		}
		if h.useTls {
			ln = tls.NewListener(ln, &tls.Config{
				GetCertificate: lc.GetCertificate,
			})
		}

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Hack since the host will include a tailnet name when using TLS.
			rh := strings.Split(r.Host, ".")[0]
			backendURL, err := c.getBackendUrl(rh, r.URL.Path)
			if err != nil {
				log.Printf("upstream server %s not found: %s", rh, err.Error())
				http.Error(w, fmt.Sprintf("upstream server %s not found", rh), http.StatusNotFound)
				return
			}
			// TODO: optional request logging
			director := func(req *http.Request) {
				req.URL = backendURL
				who, err := lc.WhoIs(req.Context(), req.RemoteAddr)
				if err != nil {
					log.Println("failed to get the owner of the request")
					return
				}
				if who.UserProfile == nil {
					log.Println("user profile is nil")
					return
				}
				req.Header.Set("X-Webauth-User", who.UserProfile.LoginName)
				req.Header.Set("X-Webauth-Name", who.UserProfile.DisplayName)
			}
			proxy := &httputil.ReverseProxy{Director: director}
			proxy.ServeHTTP(w, r)
		})

		srv := http.Server{Handler: handler}
		c.hosts[n].httpServer = &srv
		go func() {
			if err := srv.Serve(ln); err != nil {
				log.Println("failed to serve: ", err)
			}
		}()
		c.hosts[n].started = true
	}
}

func (c *controller) shutdown() {
	for n, h := range c.hosts {
		if h.started {
			log.Println("deleting host ", n)
			if err := h.httpServer.Close(); err != nil {
				log.Printf("failed to close http server: %v", err)
			}
			if err := h.tsServer.Close(); err != nil {
				log.Printf("failed to close ts server: %v", err)
			}
			delete(c.hosts, n)
		}
	}
}
