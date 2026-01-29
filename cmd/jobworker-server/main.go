package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/bucknercd/jobworker/internal/manager"
	jobpb "github.com/bucknercd/jobworker/proto/gen/jobpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ---- MAIN ----

func main() {
	var (
		listenAddr = flag.String("listen", ":50051", "listen address")
		certsDir   = flag.String("certs", "./certs", "certs directory")
		logPath    = flag.String("log", "./jobworker-server.log", "server log file")
	)
	flag.Parse()

	lf, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("open log file: %v", err)
	}

	logger := log.New(lf, "", log.LstdFlags|log.Lmsgprefix)
	logger.SetPrefix("[jobworker-server] ")

	abs, _ := filepath.Abs(*logPath)
	logger.Printf("logging to %s", abs)

	tlsCfg, err := buildServerTLSConfig(*certsDir)
	if err != nil {
		logger.Fatalf("tls config: %v", err)
	}

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		logger.Fatalf("listen %s: %v", *listenAddr, err)
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))

	mgr := manager.NewManager(logger)
	jobpb.RegisterJobWorkerServer(grpcServer, NewGRPCServer(logger, mgr))

	logger.Printf("listening on %s", *listenAddr)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Fatalf("serve: %v", err)
	}
}

// --- TLS helpers ---

func buildServerTLSConfig(certsDir string) (*tls.Config, error) {
	// server cert/key
	certPath := filepath.Join(certsDir, "server.crt")
	keyPath := filepath.Join(certsDir, "server.key")
	serverCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}

	// client CA bundle
	caPath := filepath.Join(certsDir, "ca.crt")
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read ca.crt: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if ok := clientCAs.AppendCertsFromPEM(caPEM); !ok {
		return nil, fmt.Errorf("append ca.crt: no certs found")
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},

		ClientCAs:  clientCAs,
		ClientAuth: tls.RequireAndVerifyClientCert,

		// Good hygiene
		PreferServerCipherSuites: true,
	}, nil
}
