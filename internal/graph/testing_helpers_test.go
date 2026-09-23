package graph

import "time"

// makeTimer returns a channel that fires after a generous timeout.
func makeTimer() <-chan time.Time {
	return time.After(2 * time.Second)
}
