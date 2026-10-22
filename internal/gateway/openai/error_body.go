package openai

import (
	"io"
	"time"
)

const maxUpstreamErrorBody = 64 << 10

// readUpstreamErrorBody bounds both the bytes retained and time spent reading
// an upstream error response. Cancellation interrupts the underlying wire read.
func readUpstreamErrorBody(body io.ReadCloser, cancel func(), timeout time.Duration) []byte {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	timer := time.AfterFunc(timeout, cancel)
	data, _ := io.ReadAll(io.LimitReader(body, maxUpstreamErrorBody+1))
	if !timer.Stop() || len(data) > maxUpstreamErrorBody {
		cancel()
	}
	if len(data) > maxUpstreamErrorBody {
		data = data[:maxUpstreamErrorBody]
	}
	return data
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelOnCloseBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}
