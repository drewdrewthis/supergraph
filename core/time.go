package core

import "time"

// timeLayout is the on-disk representation for Envelope.TS. RFC3339Nano keeps
// sub-second precision and is lexically sortable in UTC, so a TEXT column orders
// correctly by time as well as by id.
const timeLayout = time.RFC3339Nano

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}
