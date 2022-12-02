package main

import (
	"context"
	"github.com/bep/debounce"
	"k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type update struct {
	ingresses []*v1.Ingress
}

func listen(ctx context.Context, client kubernetes.Interface, handleUpdate func(*update)) {
	factory := informers.NewSharedInformerFactory(client, time.Minute)
	ingressLister := factory.Networking().V1().Ingresses().Lister()

	onChange := func() {
		ingresses, err := ingressLister.List(labels.Everything())
		if err != nil {
			log.Println("failed to list ingresses: ", err)
			return
		}
		handleUpdate(&update{ingresses})
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
	<-ctx.Done()
}

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal("failed to get kubernetes config:", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("failed to create kubernetes client", err)
	}

	tsAuthKey := os.Getenv("TS_AUTHKEY")
	if tsAuthKey == "" {
		log.Fatal("missing TS_AUTHKEY")
	}

	c := newController(tsAuthKey)

	ctx, cancel := context.WithCancel(context.Background())
	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-s
		log.Println("shutting down")
		cancel()
		os.Exit(0)
	}()
	listen(ctx, client, c.update)
}
