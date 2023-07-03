package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bep/debounce"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

type update struct {
	ingresses []*v1.Ingress
}

type updateConfigMap struct {
	configMaps []*corev1.ConfigMap
}

func listen(ctx context.Context, client kubernetes.Interface, handleUpdate func(*update), handleConfigMapUpdate func(*updateConfigMap)) {
	factory := informers.NewSharedInformerFactory(client, time.Minute)
	ingressLister := factory.Networking().V1().Ingresses().Lister()
	configMapLister := factory.Core().V1().ConfigMaps().Lister()

	onChange := func() {
		ingresses, err := ingressLister.List(labels.Everything())
		if err != nil {
			log.Println("failed to list ingresses: ", err)
			return
		}
		log.Printf("onChange ingress items to review=%d", len(ingresses))
		handleUpdate(&update{ingresses})
	}

	onConfigMapChange := func() {
		configMaps, err := configMapLister.List(labels.Everything())
		if err != nil {
			log.Println("failed to list config maps: ", err)
			return
		}
		log.Printf("onChange configmap")
		handleConfigMapUpdate(&updateConfigMap{configMaps})
	}

	debounced := debounce.New(time.Second)
	eventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { debounced(onChange) },
		UpdateFunc: func(any, any) { debounced(onChange) },
		DeleteFunc: func(any) { debounced(onChange) },
	}

	eventHandlerConfig := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { debounced(onConfigMapChange) },
		UpdateFunc: func(any, any) { debounced(onConfigMapChange) },
		DeleteFunc: func(any) { debounced(onConfigMapChange) },
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
	go func() {
		i := factory.Core().V1().ConfigMaps().Informer()
		i.AddEventHandler(eventHandlerConfig)
		i.Run(ctx.Done())
	}()
	<-ctx.Done()
}

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal("TIS: failed to get kubernetes config:", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("TIS: failed to create kubernetes client", err)
	}

	tsAuthKey := os.Getenv("TS_AUTHKEY")
	if tsAuthKey == "" {
		log.Fatal("TIS: missing TS_AUTHKEY")
	}

	c := NewHttpController(tsAuthKey)
	cTcp := NewTcpController(tsAuthKey)

	ctx, cancel := context.WithCancel(context.Background())
	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-s
		c.shutdown()
		cTcp.shutdown()
		log.Println("shutting down")
		cancel()
		os.Exit(0)
	}()
	listen(ctx, client, c.update, cTcp.update)
}
