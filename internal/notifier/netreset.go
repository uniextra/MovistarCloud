package notifier

import (
	"crypto/tls"
	"net/http"
	"time"
)

// transport es el de todas las peticiones a Telegram: propio y solo HTTP/1.1. Con
// HTTP/2 todos los envíos comparten una única conexión; si esa conexión muere silenciosamente
// por cortes de red o reinicios de NAT, las notificaciones y reintentos se atascan.
// Con HTTP/1.1, el reintento (tras cerrar las conexiones ociosas) abre una conexión nueva.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if t.TLSClientConfig != nil {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
	} else {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return t
}()

func newClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport}
}
