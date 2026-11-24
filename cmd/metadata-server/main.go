// metadata-server is k4all's metadata service for one VM, run in the VM's
// virt-launcher pod by the operator's admission policy:
//
//	metadata-server            serve the VM's metadata (the native sidecar)
//	metadata-server -redirect  install the 169.254.169.254 redirect (the init container)
//	metadata-server -probe     ask the running server (its exec probe)
//
// Static, no API access: see internal/metadata/server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/nftables"

	"github.com/gpillon/k4all-operator/internal/metadata"
	mdredirect "github.com/gpillon/k4all-operator/internal/metadata/redirect"
	"github.com/gpillon/k4all-operator/internal/metadata/server"
)

func main() {
	probe := flag.Bool("probe", false, "ask the running server and exit 0 if it answers (exec probe)")
	install := flag.Bool("redirect", false,
		"install the redirect of 169.254.169.254:80 in this network namespace and exit; "+
			"a failure is logged and exits 0, the VM then runs without the service")
	listen := flag.String("listen", fmt.Sprintf(":%d", metadata.Port), "address the guest's requests are redirected to")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	switch {
	case *probe:
		if err := server.Probe(server.HealthSocket, 3*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *install:
		// Exits 0 either way: a broken redirect must not stop the VM.
		redirect(func() error {
			c, err := nftables.New()
			if err != nil {
				return err
			}
			return mdredirect.Install(c, metadata.Port)
		}, log.Printf)
	default:
		if err := serve(*listen); err != nil {
			log.Fatal(err)
		}
	}
}

// redirect runs install and logs how it went; a failure is not an error of
// the init container: the VM must start, without the service.
func redirect(install func() error, logf func(format string, args ...any)) {
	if err := install(); err != nil {
		logf("redirect of %s:%d not installed, this VM runs without the metadata service: %v",
			metadata.Address, metadata.GuestPort, err)
		return
	}
	logf("redirect of %s:%d from %s to :%d installed", metadata.Address, metadata.GuestPort,
		metadata.GuestInterface, metadata.Port)
}

func serve(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	health, err := server.ServeHealth(server.HealthSocket)
	if err != nil {
		return fmt.Errorf("health socket: %w", err)
	}
	defer func() { _ = health.Close() }()

	srv := &http.Server{
		Handler: server.New(server.Config{
			PodDir: metadata.PodDir, VMDir: metadata.VMDir, VendorDir: metadata.VendorDir,
			TokenDir: metadata.TokenDir, ARPFile: "/proc/net/arp", Logf: log.Printf,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("serving %s:%d on %s", metadata.Address, metadata.GuestPort, addr)
	err = srv.Serve(server.RedirectedOnly(l, server.OriginalDestination, log.Printf))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
