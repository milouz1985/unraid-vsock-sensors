package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	device "unraid-vsock-sensors/coolercontrol-plugin/gen/device_service"

	"google.golang.org/grpc"
)

const (
	serviceID = "unraid-vsock-sensors-cc"
	version   = "0.1.0"
)

func main() {
	cid := flag.Uint("cid", 3, "AF_VSOCK CID of the Unraid VM")
	port := flag.Uint("port", 19090, "unraid-vsock-sensors port")
	configFile := flag.String("config", "", "plugin config.json (default: next to executable)")
	socket := flag.String("socket", "/tmp/unraid-vsock-sensors-cc.sock", "CoolerControl plugin Unix socket")
	flag.Parse()
	if uint64(*cid) > uint64(^uint32(0)) || uint64(*port) > uint64(^uint32(0)) {
		log.Fatal("cid and port must fit in 32 bits")
	}
	path, err := configPath(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	config, err := loadConfig(path, runtimeConfig{CID: uint32(*cid), Port: uint32(*port)})
	if err != nil {
		log.Fatal(err)
	}

	listener, err := listenUnixSocket(*socket)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(*socket)

	server := grpc.NewServer()
	device.RegisterDeviceServiceServer(server, newUnraidService(config.CID, config.Port))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		server.GracefulStop()
	}()

	log.Printf("starting %s v%s on %s (vsock %d:%d)", serviceID, version, *socket, config.CID, config.Port)
	if err := server.Serve(listener); err != nil {
		log.Fatal(fmt.Errorf("serve gRPC: %w", err))
	}
}
