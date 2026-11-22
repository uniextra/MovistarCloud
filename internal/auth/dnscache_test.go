package auth

import (
	"net"
	"testing"
	"time"
)

func TestDNSCache(t *testing.T) {
	host := "cloud.movistar.es"
	addrs := []net.IPAddr{
		{IP: net.ParseIP("1.2.3.4")},
		{IP: net.ParseIP("5.6.7.8")},
	}

	// 1. Initially empty
	dnsCacheDrop(host)
	if _, ok := dnsCacheGet(host, time.Now()); ok {
		t.Fatal("expected cache miss for dropped host")
	}

	// 2. Cache put and hit
	dnsCachePut(host, addrs, 10*time.Second)
	cached, ok := dnsCacheGet(host, time.Now())
	if !ok || len(cached) != 2 {
		t.Fatalf("expected cache hit with 2 addrs, got ok=%v, addrs=%v", ok, cached)
	}

	// 3. Expiry
	future := time.Now().Add(11 * time.Second)
	if _, ok := dnsCacheGet(host, future); ok {
		t.Fatal("expected cache miss for expired entry")
	}

	// 4. Drop
	dnsCacheDrop(host)
	if _, ok := dnsCacheGet(host, time.Now()); ok {
		t.Fatal("expected cache miss after drop")
	}
}
