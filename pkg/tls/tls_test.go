/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package tls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"testing"a
	"time"

	"chainguard.dev/go-grpc-kit/pkg/duplex"
	pb "chainguard.dev/go-grpc-kit/pkg/tls/internal/proto/helloworld"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
	"google.golang.org/grpc/metadata"
)

func TestTLS(t *testing.T) {
	ctx := context.Background()

	// We need to init the listener first so that we can generate a TLS cert
	// that's bound to the listening IP.
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Logf("server listening at %v", lis.Addr())

	// Get IP address from listener
	ip, err := net.ResolveTCPAddr(lis.Addr().Network(), lis.Addr().String())
	if err != nil {
		t.Fatalf("error resolving IP address: %v", err)
	}

	tlsConfig, err := generateTLS(&x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotAfter:     time.Now().Add(10 * time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{ip.IP},
	})
	if err != nil {
		t.Fatalf("error generating certificate")
	}

	// Start server
	d := duplex.New(ip.Port,
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
	)
	pb.RegisterGreeterServer(d.Server, &server{})
	if err := d.RegisterHandler(ctx, pb.RegisterGreeterHandlerFromEndpoint); err != nil {
		t.Fatalf("error registering handler: %v", err)
	}
	go func() {
		if err := d.Serve(ctx, tls.NewListener(lis, tlsConfig)); err != nil {
			panic(fmt.Sprintf("failed to serve: %v", err))
		}
	}()

	// grpc client
	conn, err := grpc.Dial(lis.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		// Send password to verify gRPC doesn't reject it.
		grpc.WithPerRPCCredentials(oauth.TokenSource{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "hunter2"})}),
	)
	if err != nil {
		log.Fatalf("failed to dial: %v", err)
	}
	client := pb.NewGreeterClient(conn)
	req := &pb.HelloRequest{
		Name: "world",
	}
	resp, err := client.SayHello(ctx, req)
	if err != nil {
		t.Fatalf("grpc request failed: %v", err)
	}
	t.Log("grpc response:", resp)

	// http client
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal json: %v", err)
	}
	url := "https://" + lis.Addr().String() + "/v1/example/echo"
	t.Log(url)
	httpResp, err := httpClient.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Fatalf("http request failed: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Fatalf("non-zero HTTP response code: %s", httpResp.Status)
	}
	b, _ := io.ReadAll(httpResp.Body)
	t.Log("http response:", string(b))
}

// Generate TLS creates a TLS config with a self-signed cert based on tmpl.
func generateTLS(tmpl *x509.Certificate) (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("error generating private key: %w", err)
	}
	pub := &priv.PublicKey
	raw, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("error generating certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: raw,
	})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("error marshaling key bytes: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: keyBytes,
	})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("error loading tls certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("error adding cert to pool")
	}

	// configuration of the certificate what we want to
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		RootCAs:      pool,
	}, nil
}

// server is used to implement helloworld.GreeterServer.
type server struct {
	pb.UnimplementedGreeterServer
}

// SayHello implements helloworld.GreeterServer
func (s *server) SayHello(ctx context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	log.Printf("Received: %v (%v)", in.GetName(), md)
	return &pb.HelloReply{Message: "Hello " + in.GetName()}, nil
}
