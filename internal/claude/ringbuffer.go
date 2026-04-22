package claude

import "sync"

// ringBuffer is a fixed-capacity byte buffer that overwrites oldest data on
// overflow. Used to capture stderr without unbounded growth.
type ringBuffer struct {
	mu   sync.Mutex
	buf  []byte
	cap  int
	full bool
	w    int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, capacity), cap: capacity}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range p {
		r.buf[r.w] = b
		r.w++
		if r.w == r.cap {
			r.w = 0
			r.full = true
		}
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return string(r.buf[:r.w])
	}
	out := make([]byte, 0, r.cap)
	out = append(out, r.buf[r.w:]...)
	out = append(out, r.buf[:r.w]...)
	return string(out)
}
