package sapi

import (
	"io"
	"sync"
	"time"
)

// stallReader vigila que la conexión con el servidor siga leyendo el cuerpo del archivo.
// Si pasa timeout sin pedir más datos y no hay ninguna lectura en curso, ejecuta onStall (cancela el contexto).
type stallReader struct {
	r       io.Reader
	mu      sync.Mutex
	last    time.Time
	reading bool
	ended   bool // cuerpo transferido entero
	stalled bool
	done    chan struct{}
	once    sync.Once
}

func newStallReader(r io.Reader, timeout time.Duration, onStall func()) *stallReader {
	s := &stallReader{r: r, last: time.Now(), done: make(chan struct{})}
	go func() {
		t := time.NewTicker(timeout / 6)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				s.mu.Lock()
				idle := !s.reading && !s.ended && time.Since(s.last) > timeout
				if idle {
					s.stalled = true
				}
				s.mu.Unlock()
				if idle {
					onStall()
					return
				}
			}
		}
	}()
	return s
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	s.reading = true
	s.mu.Unlock()

	n, err := s.r.Read(p)

	s.mu.Lock()
	s.reading = false
	s.last = time.Now()
	if err != nil {
		s.ended = true
	}
	s.mu.Unlock()

	return n, err
}

func (s *stallReader) stop() bool {
	s.once.Do(func() { close(s.done) })
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stalled
}
