package main

import (
	"errors"
	"time"
)

// Go's time.Parse accepts zone offsets such as +24:00 and +00:60 even though
// RFC3339 permits only hours 00..23 and minutes 00..59. Validate the offset
// explicitly so malformed selectors never reach a retrieval or history gate.
func parseStrictRFC3339Timestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	if value[len(value)-1] == 'Z' {
		return parsed, nil
	}
	if len(value) < 6 {
		return time.Time{}, errors.New("invalid RFC3339 timezone offset")
	}
	zone := value[len(value)-6:]
	if (zone[0] != '+' && zone[0] != '-') || zone[3] != ':' {
		return time.Time{}, errors.New("invalid RFC3339 timezone offset")
	}
	hour := int(zone[1]-'0')*10 + int(zone[2]-'0')
	minute := int(zone[4]-'0')*10 + int(zone[5]-'0')
	if hour > 23 || minute > 59 {
		return time.Time{}, errors.New("RFC3339 timezone offset out of range")
	}
	return parsed, nil
}
