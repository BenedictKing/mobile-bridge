package command

import (
	"context"
	"sync"
)

type slot struct {
	token chan struct{}
	refs  int
}

// Serial is a non-reentrant keyed lock. Entries retire after the last waiter.
type Serial struct {
	mu    sync.Mutex
	slots map[string]*slot
}

func (s *Serial) Lock(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.slots == nil {
		s.slots = map[string]*slot{}
	}
	v := s.slots[key]
	if v == nil {
		v = &slot{token: make(chan struct{}, 1)}
		v.token <- struct{}{}
		s.slots[key] = v
	}
	v.refs++
	s.mu.Unlock()
	retire := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		v.refs--
		if v.refs == 0 {
			delete(s.slots, key)
		}
	}
	select {
	case <-ctx.Done():
		retire()
		return nil, ctx.Err()
	case <-v.token:
		var once sync.Once
		unlock := func() { once.Do(func() { v.token <- struct{}{}; retire() }) }
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	}
}
