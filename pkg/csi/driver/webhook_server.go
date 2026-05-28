package driver

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"k8s.io/klog/v2"
)

const (
	webhookCertDir  = "/tls/webhook"
	webhookCertFile = "tls.crt"
	webhookKeyFile  = "tls.key"
)

func (d *Driver) maybeStartWebhookServer(ctx context.Context) {
	if d == nil || strings.TrimSpace(d.nodeID) != "" || !d.lastNodePreferenceWebhookEnabled() {
		return
	}

	port, ok := d.PluginConfig.GetInt(config.LastNodePreferenceWebhookPortVar)
	if !ok || port <= 0 {
		port = 9443
	}
	addr := ":" + strconv.Itoa(port)
	certPath := filepath.Join(webhookCertDir, webhookCertFile)
	keyPath := filepath.Join(webhookCertDir, webhookKeyFile)
	if _, err := os.Stat(certPath); err != nil {
		klog.V(2).InfoS("Skipping last-node preference webhook start because certificate is unavailable", "certPath", certPath, "err", err)
		return
	}
	if _, err := os.Stat(keyPath); err != nil {
		klog.V(2).InfoS("Skipping last-node preference webhook start because key is unavailable", "keyPath", keyPath, "err", err)
		return
	}

	server := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			if req.URL.Path != lastNodePreferenceWebhookPath {
				http.NotFound(rw, req)
				return
			}
			NewLastNodePreferenceWebhook(d).ServeHTTP(rw, req)
		}),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		klog.InfoS("Starting last-node preference webhook server", "addr", addr, "path", lastNodePreferenceWebhookPath)
		if err := server.ListenAndServeTLS(certPath, keyPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.ErrorS(err, "Last-node preference webhook server failed")
		}
	}()
}

func (d *Driver) lastNodePreferenceWebhookEnabled() bool {
	if d == nil {
		return false
	}
	featureEnabled, ok := d.PluginConfig.GetBool(config.LastNodePreferenceEnabledVar)
	if ok && !featureEnabled {
		return false
	}
	webhookEnabled, ok := d.PluginConfig.GetBool(config.LastNodePreferenceWebhookEnabledVar)
	if !ok {
		return true
	}
	return webhookEnabled
}
