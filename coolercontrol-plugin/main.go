// Command unraid-vsock-sensors-cc exposes Unraid temperatures to CoolerControl.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	device "unraid-vsock-sensors/coolercontrol-plugin/gen/device_service"

	"google.golang.org/grpc"
)

const (
	serviceID  = "unraid-vsock-sensors-cc"
	socketPath = "/tmp/unraid-vsock-sensors-cc.sock"
)

var version = "dev"

func main() {
	path, err := configPath()
	if err != nil {
		log.Fatal(err)
	}
	config, err := loadConfig(path)
	if err != nil {
		log.Fatal(err)
	}

	listener, err := listenUnixSocket(socketPath)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	server := grpc.NewServer()
	device.RegisterDeviceServiceServer(server, newUnraidService(config.CID, config.Port))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		server.GracefulStop()
	}()

	log.Printf("starting %s v%s on %s (vsock %d:%d)", serviceID, version, socketPath, config.CID, config.Port)
	if err := server.Serve(listener); err != nil {
		log.Fatal(fmt.Errorf("serve gRPC: %w", err))
	}
}
