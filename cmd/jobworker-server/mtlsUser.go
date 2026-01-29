package main

import (
	"context"
	"fmt"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func mtlsUserFromContext(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "", fmt.Errorf("no peer auth info")
	}

	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", fmt.Errorf("unexpected auth info type: %T", p.AuthInfo)
	}

	if len(ti.State.PeerCertificates) == 0 {
		return "", fmt.Errorf("no peer certificates")
	}

	cn := ti.State.PeerCertificates[0].Subject.CommonName
	if cn == "" {
		return "", fmt.Errorf("peer cert CN is empty")
	}
	return cn, nil
}
