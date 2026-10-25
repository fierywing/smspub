package main

import (
	"crypto/tls"
	"errors"
	"net/http"
	"time"
)

// An optional second listener allows browser microphone access on LAN hosts
// while preserving the existing HTTP endpoint and its callers.
func httpsServer(addr, certFile, keyFile string, handler http.Handler) (*http.Server, error) {
	if addr == "" && certFile == "" && keyFile == "" {
		return nil, nil
	}
	if addr == "" || certFile == "" || keyFile == "" {
		return nil, errors.New("HTTPS 需要同时指定 -https-listen、-tls-cert 和 -tls-key")
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}}, nil
}
