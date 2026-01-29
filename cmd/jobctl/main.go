package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	jobpb "github.com/bucknercd/jobworker/proto/gen/jobpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:50051", "jobworker server address")
		certsDir = flag.String("certs", "./certs", "certs directory")
		cmd      = flag.String("cmd", "", "command: start|status|stop|stream")
		jobID    = flag.String("id", "", "job id for status/stop/stream")
		target   = flag.String("target", "stdout", "stream target: stdout|stderr")
		insecure = flag.Bool("insecure", false, "skip TLS verification (dev only)")
		// start params
		exe  = flag.String("exe", "", "executable for start (e.g. ls or /bin/ls)")
		args = flag.String("args", "", "args for start, single string (e.g. \"-lah /\")")
		cpu  = flag.String("cpu", "", "cpu limit (e.g. 500m, 2, max)")
		mem  = flag.String("mem", "", "memory limit (e.g. 100M, max)")
		ioCl = flag.String("io", "", "io class (low|med|high)")
	)
	flag.Parse()

	if *cmd == "" {
		die("missing -cmd (start|status|stop|stream)")
	}

	tlsCfg, err := buildClientTLSConfig(*certsDir, *addr, *insecure)
	if err != nil {
		die("tls config: %v", err)
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		die("dial %s: %v", *addr, err)
	}
	defer conn.Close()

	client := jobpb.NewJobWorkerClient(conn)

	switch *cmd {
	case "start":
		if *exe == "" {
			die("start requires -exe")
		}
		// NOTE: args parsing is minimal; later swap to cobra and proper arg splitting.
		req := &jobpb.StartJobRequest{
			Executable: *exe,
			Args:       splitArgs(*args),
			Limits: &jobpb.ResourceLimits{
				Cpu:       *cpu,
				MemoryMax: *mem,
				IoClass:   *ioCl,
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		resp, err := client.StartJob(ctx, req)
		if err != nil {
			die("StartJob: %v", err)
		}
		fmt.Println(resp.GetJobId())

	case "status":
		if *jobID == "" {
			die("status requires -id")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, err := client.GetStatus(ctx, &jobpb.GetStatusRequest{JobId: *jobID})
		if err != nil {
			die("GetStatus: %v", err)
		}
		fmt.Printf("job_id=%s status=%s exit_code=%d\n",
			resp.GetJobId(),
			resp.GetMetadata().GetStatus().String(),
			resp.GetMetadata().GetExitCode(),
		)

	case "stop":
		if *jobID == "" {
			die("stop requires -id")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		resp, err := client.StopJob(ctx, &jobpb.StopJobRequest{JobId: *jobID})
		if err != nil {
			die("StopJob: %v", err)
		}
		fmt.Printf("status=%s exit_code=%d\n",
			resp.GetMetadata().GetStatus().String(),
			resp.GetMetadata().GetExitCode(),
		)

	case "stream":
		if *jobID == "" {
			die("stream requires -id")
		}

		var t jobpb.StreamTarget
		switch *target {
		case "stdout":
			t = jobpb.StreamTarget_STREAM_TARGET_STDOUT
		case "stderr":
			t = jobpb.StreamTarget_STREAM_TARGET_STDERR
		default:
			die("invalid -target (stdout|stderr)")
		}

		ctx := context.Background()
		stream, err := client.StreamOutput(ctx, &jobpb.StreamOutputRequest{
			JobId:  *jobID,
			Target: t,
		})
		if err != nil {
			die("StreamOutput: %v", err)
		}

		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					return
				}
				die("stream recv: %v", err)
			}
			os.Stdout.Write(msg.GetChunk())
		}

	default:
		die("unknown -cmd: %s", *cmd)
	}
}

func die(format string, args ...any) {
	log.Printf(format, args...)
	os.Exit(1)
}

func splitArgs(s string) []string {
	// minimal: split on spaces (no quotes handling)
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	inSpace := true
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if !inSpace {
				out = append(out, cur)
				cur = ""
			}
			inSpace = true
			continue
		}
		inSpace = false
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// func buildClientTLSConfig(certsDir, user, addr string) (*tls.Config, error) {
// 	// client cert
// 	clientCertPath := filepath.Join(certsDir, user, "client.crt")
// 	clientKeyPath := filepath.Join(certsDir, user, "client.key")
// 	clientCert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
// 	if err != nil {
// 		return nil, fmt.Errorf("load client keypair: %w", err)
// 	}

// 	// trust server CA
// 	caPath := filepath.Join(certsDir, "ca.crt")
// 	caPEM, err := os.ReadFile(caPath)
// 	if err != nil {
// 		return nil, fmt.Errorf("read ca.crt: %w", err)
// 	}
// 	roots := x509.NewCertPool()
// 	if ok := roots.AppendCertsFromPEM(caPEM); !ok {
// 		return nil, fmt.Errorf("append ca.crt: no certs found")
// 	}

// 	host := addr
// 	// addr might be "host:port"
// 	if h, _, err := net.SplitHostPort(addr); err == nil {
// 		host = h
// 	}

// 	return &tls.Config{
// 		MinVersion:   tls.VersionTLS13,
// 		Certificates: []tls.Certificate{clientCert},
// 		RootCAs:      roots,

// 		// IMPORTANT:
// 		// Your server cert CN is "mtls-server". If you don't set ServerName,
// 		// Go may fail verification depending on how you connect.
// 		ServerName: host,
// 	}, nil
// }

func buildClientTLSConfig(certsDir, addr string, insecure bool) (*tls.Config, error) {
	identityDir, err := discoverIdentityDir(certsDir)
	if err != nil {
		return nil, err
	}

	clientCertPath := filepath.Join(identityDir, "client.crt")
	clientKeyPath := filepath.Join(identityDir, "client.key")
	clientCert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client keypair (%s): %w", identityDir, err)
	}

	caPath := filepath.Join(certsDir, "ca.crt")
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read ca.crt: %w", err)
	}
	roots := x509.NewCertPool()
	if ok := roots.AppendCertsFromPEM(caPEM); !ok {
		return nil, fmt.Errorf("append ca.crt: no certs found")
	}

	host := addr
	// addr might be "host:port"
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}

	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      roots,
		ServerName:   host,
	}

	if insecure {
		tlsCfg.InsecureSkipVerify = true // dev-only
	}
	return tlsCfg, nil
}

func discoverIdentityDir(certsDir string) (string, error) {
	entries, err := os.ReadDir(certsDir)
	if err != nil {
		return "", err
	}

	var found string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(certsDir, e.Name())
		if fileExists(filepath.Join(d, "client.crt")) && fileExists(filepath.Join(d, "client.key")) {
			if found != "" {
				return "", fmt.Errorf("multiple identities found under %s; specify one", certsDir)
			}
			found = d
		}
	}
	if found == "" {
		return "", fmt.Errorf("no identity found under %s", certsDir)
	}
	return found, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
