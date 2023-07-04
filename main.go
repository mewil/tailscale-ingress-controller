package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

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

	cHttp := NewHttpController(tsAuthKey)
	cTcp := NewTcpController(tsAuthKey)

	ctx, cancel := context.WithCancel(context.Background())
	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-s
		cHttp.shutdown()
		cTcp.shutdown()
		log.Println("shutting down")
		cancel()
		os.Exit(0)
	}()

	go cHttp.listen(ctx, client)
	go cTcp.listen(ctx, client)
	<-s
}
