package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bep/debounce"
	"inet.af/tcpproxy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"tailscale.com/ipn/store/kubestore"
	"tailscale.com/tsnet"
)

// TcpController state
type TcpController struct {
	// Tailscale auth key
	tsAuthKey string
	// Mutex to control access to shared hosts structure
	mu sync.RWMutex
	// Map of TcpHost proxies
	hosts map[string]*TcpHost
}

// An individual TCP proxy server
type TcpHost struct {
	// Tailscale leg of the proxy
	tsServer *tsnet.Server
	// Backend service proxy
	proxy *tcpproxy.Proxy
	// ConfigMap parameters signature to check
	// if configuration was updated
	signature string
}

// Create a new controller with a specified tsAuthKey for Tailscale
func NewTcpController(tsAuthKey string) *TcpController {
	return &TcpController{
		tsAuthKey: tsAuthKey,
		mu:        sync.RWMutex{},
		hosts:     make(map[string]*TcpHost),
	}
}

// Update controller state with the data from ConfigMap
func (c *TcpController) update(payload *updateConfigMap) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, configMap := range payload.configMaps {
		if configMap.Name != os.Getenv("TCP_SERVICES_CONFIGMAP") {
			continue
		}

		aliveHosts := make(map[string]bool)

		// go through the ConfigMap to re-create services that were changed
		for sourceSpec, targetSpec := range configMap.Data {
			// tailnet-host-name.port
			tailnetHost, tailnetPort, ok := strings.Cut(sourceSpec, ".")
			if !ok {
				log.Printf("TIC: Invalid tailnet spec [%s], must be <host>.<port> format", sourceSpec)
				continue
			}
			// [namespace/]service:port
			targetServiceRef, targetPort, ok := strings.Cut(targetSpec, ":")
			if !ok {
				log.Printf("TIC: Invalid target spec [%s], must be [<namespace>/]<service>:<port> format", sourceSpec)
				continue
			}

			aliveHosts[sourceSpec] = true

			oldHost, ok := c.hosts[sourceSpec]

			if ok {
				// there is already a TCP proxy host with this name
				if oldHost.signature != fmt.Sprintf("%s: %s", sourceSpec, targetSpec) {
					// if host signature does not match — re-create
					log.Printf("TIC: Host [%s] was updated, re-creating", sourceSpec)
					oldHost.proxy.Close()
					oldHost.tsServer.Close()
					delete(c.hosts, tailnetHost)
				} else {
					// skip host if signature is the same
					log.Printf("TIC: Host [%s] was not changed, skipping", sourceSpec)
					continue
				}
			}

			// construct target service address
			var targetAddress string
			var fullTargetAddress *string

			targetNamespace, targetService, found := strings.Cut(targetServiceRef, "/")
			if found {
				// generate FQDN
				targetAddress = fmt.Sprintf("%s.%s.svc.cluster.local", targetService, targetNamespace)
			} else {
				// assume same namespace
				targetAddress = targetServiceRef
			}

			fullTargetAddress, err := resolveTargetAddress(targetAddress, targetPort)

			if err != nil {
				log.Printf("TIC: unable to resolve target address %v", err)
				continue
			}

			dir, err := generateTsDir("tsproxy", tailnetHost)

			if err != nil {
				log.Printf("TIC: Unable to create dir for tsnet: %s", err.Error())
				continue
			}

			kubeStore, err := kubestore.New(log.Printf, fmt.Sprintf("tsproxy-%s", tailnetHost))

			if err != nil {
				log.Printf("TIC: unable to create kubestore: %s", err.Error())
			}

			// initialize tsnet
			tsServer := &tsnet.Server{
				Dir:       *dir,
				Hostname:  tailnetHost,
				Ephemeral: true,
				AuthKey:   c.tsAuthKey,
				Logf:      nil,
				Store:     kubeStore,
			}

			// setup proxy
			proxy := &tcpproxy.Proxy{
				ListenFunc: func(net, laddr string) (net.Listener, error) {
					return tsServer.Listen(net, laddr)
				},
			}

			signature := fmt.Sprintf("%s: %s", sourceSpec, targetSpec)

			c.hosts[sourceSpec] = &TcpHost{
				tsServer,
				proxy,
				signature,
			}
			proxy.AddRoute(":"+tailnetPort, tcpproxy.To(*fullTargetAddress))

			// launch a dedicated goroutine with the proxy
			go func() {
				log.Printf("TIC: Starting TCP proxy %s:%s -> %s", tailnetHost, tailnetPort, *fullTargetAddress)
				proxy.Run()
			}()
		}

		// remove hosts that are no longer present in the ConfigMap
		for idx, host := range c.hosts {
			if _, ok := aliveHosts[idx]; !ok {
				log.Printf("TIC: host [%s] no longer alive in ConfigMap, removing", idx)
				// if host was not found in the alive hosts
				host.proxy.Close()
				host.tsServer.Close()
				delete(c.hosts, idx)
			}
		}
	}
}

// Shutdown all TCP proxy connections and listeners.
func (c *TcpController) shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// shutdown TCP proxies
	for idx, tcpHost := range c.hosts {
		if err := tcpHost.proxy.Close(); err != nil {
			log.Printf("Unable to close TCP proxy: %v", err)
		}
		if err := tcpHost.tsServer.Close(); err != nil {
			log.Printf("Unable to close ts server: %v", err)
		}
		delete(c.hosts, idx)
	}
}

type updateConfigMap struct {
	configMaps []*corev1.ConfigMap
}

func (c *TcpController) listen(ctx context.Context, client kubernetes.Interface) {
	factory := informers.NewSharedInformerFactory(client, time.Minute)
	configMapLister := factory.Core().V1().ConfigMaps().Lister()

	onConfigMapChange := func() {
		configMaps, err := configMapLister.List(labels.Everything())
		if err != nil {
			log.Println("failed to list config maps: ", err)
			return
		}
		log.Printf("onChange configmap")
		c.update(&updateConfigMap{configMaps})
	}
	debounced := debounce.New(time.Second)

	eventHandlerConfig := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { debounced(onConfigMapChange) },
		UpdateFunc: func(any, any) { debounced(onConfigMapChange) },
		DeleteFunc: func(any) { debounced(onConfigMapChange) },
	}

	go func() {
		i := factory.Core().V1().ConfigMaps().Informer()
		i.AddEventHandler(eventHandlerConfig)
		i.Run(ctx.Done())
	}()
	<-ctx.Done()
}
