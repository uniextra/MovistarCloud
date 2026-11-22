package sapi

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStallReader(t *testing.T) {
	var fired int32
	s := newStallReader(strings.NewReader("hola"), 60*time.Millisecond, func() { atomic.StoreInt32(&fired, 1) })
	time.Sleep(150 * time.Millisecond) // nadie lee: debe saltar watchdog
	if !s.stop() || atomic.LoadInt32(&fired) != 1 {
		t.Fatal("debía detectar que no se lee")
	}

	atomic.StoreInt32(&fired, 0)
	pr, pw := io.Pipe()
	s = newStallReader(pr, 60*time.Millisecond, func() { atomic.StoreInt32(&fired, 1) })
	go func() { time.Sleep(150 * time.Millisecond); _, _ = pw.Write([]byte("x")); _ = pw.Close() }()
	_, _ = io.ReadAll(s)
	time.Sleep(150 * time.Millisecond) // cuerpo ya completado: no debe saltar watchdog
	if s.stop() || atomic.LoadInt32(&fired) != 0 {
		t.Fatal("no debía cortar: el origen terminó de enviar")
	}
}
