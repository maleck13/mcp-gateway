// main implements the CLI for the MCP broker.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	extProcV3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	mcpRouter "github.com/kagenti/mcp-gateway/internal/mcp-router"
	"google.golang.org/grpc"
)

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func main() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	brokerServer := setUpBroker()
	routerServer := setUpRouter()

	grpcAddr := getEnv("SERVER_ADDRESS", "0.0.0.0:9002")
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("[grpc] listen error: %v", err)
	}

	go func() {
		log.Printf("[grpc] starting MCP Router listening on %s", grpcAddr)
		log.Fatal(routerServer.Serve(lis))
	}()

	go func() {
		log.Printf("[http] starting MCP Broker listening on %s", brokerServer.Addr)
		if err := brokerServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[http] %v", err)
		}
	}()

	<-stop
	// handle shutdown
	log.Printf("shutting down MCP Broker and MCP Router")
	shutdownCtx, shutdownRelease := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownRelease()
	if err := brokerServer.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("HTTP shutdown error: %v", err)
	}
	routerServer.GracefulStop()
}

func setUpBroker() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "Hello, World!")
	})
	httpSrv := &http.Server{
		Addr:         getEnv("HTTP_ADDR", ":8080"),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	return httpSrv
}

func setUpRouter() *grpc.Server {
	grpcSrv := grpc.NewServer()
	extProcV3.RegisterExternalProcessorServer(grpcSrv, &mcpRouter.ExtProcServer{})
	return grpcSrv
}
