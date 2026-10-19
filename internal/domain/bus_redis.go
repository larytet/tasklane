package domain

import "github.com/larytet/tasklane/internal/redis"

type redisBus struct{ *redis.Bus }

// RedisBus adapts a Redis (or Valkey) connection to the service's event bus.
func RedisBus(b *redis.Bus) EventBus { return redisBus{b} }

func (r redisBus) BusStatus() BusStatus {
	st := r.Bus.Status()
	return BusStatus{Connected: st.Connected, Since: st.Since, LastError: st.LastError, Dropped: st.Dropped, Received: st.Received, Published: st.Published}
}
